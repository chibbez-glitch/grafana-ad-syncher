package syncer

import (
	"crypto/rand"
	"fmt"
	"io"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"grafana-ad-syncher/internal/entra"
	"grafana-ad-syncher/internal/grafana"
	"grafana-ad-syncher/internal/store"
)

type Syncer struct {
	store            *store.Store
	grafana          *grafana.Client
	entra            *entra.Client
	defaultUserRole  string
	allowCreateUsers bool
	allowRemoveUsers bool

	mu          sync.Mutex
	lastRun     time.Time
	lastMessage string
}

type Action struct {
	ActionType      string
	OrgID           int64
	GrafanaOrgID    int64
	TeamID          int64
	TeamName        string
	UserID          int64
	Email           string
	DisplayName     string
	Role            string
	ExternalGroupID string
	Note            string
}

func New(store *store.Store, grafana *grafana.Client, entra *entra.Client, defaultRole string, allowCreateUsers bool, allowRemoveUsers bool) *Syncer {
	return &Syncer{
		store:            store,
		grafana:          grafana,
		entra:            entra,
		defaultUserRole:  defaultRole,
		allowCreateUsers: allowCreateUsers,
		allowRemoveUsers: allowRemoveUsers,
	}
}

func (s *Syncer) LastRun() (time.Time, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastRun, s.lastMessage
}

func (s *Syncer) RecordRun(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastRun = time.Now()
	if err != nil {
		s.lastMessage = err.Error()
	} else {
		s.lastMessage = "ok"
	}
}

func (s *Syncer) Run() error {
	start := time.Now()
	log.Printf("sync: starting")

	plan, err := s.BuildPlan()
	if err != nil {
		return s.finish(start, err)
	}
	if err := s.ApplyPlan(plan.Actions); err != nil {
		return s.finish(start, err)
	}
	return s.finish(start, nil)
}

func (s *Syncer) ApplyPlan(actions []store.PlanAction) error {
	if len(actions) == 0 {
		return nil
	}
	sortActions(actions)
	userIDs := map[string]int64{}
	teamIDs := map[string]int64{}

	for _, action := range actions {
		email := action.Email
		switch action.ActionType {
		case "create_team":
			teamID, err := s.grafana.EnsureTeam(action.GrafanaOrgID, action.TeamName)
			if err != nil {
				return err
			}
			teamIDs[teamKey(action.OrgID, action.TeamName)] = teamID
			if err := s.store.UpdateMappingTeamIDForName(action.OrgID, action.TeamName, teamID); err != nil {
				log.Printf("sync: update team id for %s failed: %v", action.TeamName, err)
			}
		case "create_user":
			name := action.DisplayName
			if name == "" {
				name = email
			}
			created, err := s.grafana.CreateUser(email, email, name, randomPassword())
			if err != nil {
				return err
			}
			userIDs[email] = created.ID
		case "add_user_to_org":
			if err := s.grafana.AddUserToOrg(action.GrafanaOrgID, email, action.Role); err != nil {
				return err
			}
		case "update_user_role":
			id := action.UserID
			if id == 0 {
				id = userIDs[email]
			}
			if id == 0 {
				user, found, err := s.grafana.LookupUser(email)
				if err != nil {
					return err
				}
				if found {
					id = user.ID
				}
			}
			if id != 0 {
				if err := s.grafana.UpdateUserRole(action.GrafanaOrgID, id, action.Role); err != nil {
					return err
				}
			}
		case "add_user_to_team":
			teamID := action.TeamID
			if teamID == 0 {
				teamID = teamIDs[teamKey(action.OrgID, action.TeamName)]
			}
			if teamID == 0 {
				return fmt.Errorf("missing team id for %s", action.TeamName)
			}
			id := action.UserID
			if id == 0 {
				id = userIDs[email]
			}
			if id == 0 {
				user, found, err := s.grafana.LookupUser(email)
				if err != nil {
					return err
				}
				if found {
					id = user.ID
				}
			}
			if id != 0 {
				if err := s.grafana.AddUserToTeam(teamID, id); err != nil {
					return err
				}
			}
		case "remove_user_from_team":
			teamID := action.TeamID
			if teamID == 0 {
				teamID = teamIDs[teamKey(action.OrgID, action.TeamName)]
			}
			if teamID == 0 {
				return fmt.Errorf("missing team id for %s", action.TeamName)
			}
			id := action.UserID
			if id == 0 {
				user, found, err := s.grafana.LookupUser(email)
				if err != nil {
					return err
				}
				if found {
					id = user.ID
				}
			}
			if id != 0 {
				if err := s.grafana.RemoveUserFromTeam(teamID, id); err != nil {
					return err
				}
			}
		default:
			continue
		}
	}
	return nil
}

func (s *Syncer) BuildPlan() (*store.Plan, error) {
	orgs, err := s.store.ListOrgs()
	if err != nil {
		return nil, fmt.Errorf("list orgs: %w", err)
	}
	orgByID := make(map[int64]store.Org, len(orgs))
	for _, org := range orgs {
		orgByID[org.ID] = org
	}

	mappings, err := s.store.ListMappings()
	if err != nil {
		return nil, fmt.Errorf("list mappings: %w", err)
	}

	var actions []store.PlanAction
	userCache := map[string]*grafana.User{}
	roleByOrgEmail := map[int64]map[string]string{}

	for _, mapping := range mappings {
		org, ok := orgByID[mapping.OrgID]
		if !ok {
			log.Printf("sync: mapping %d references missing org %d", mapping.ID, mapping.OrgID)
			continue
		}

		teamID := mapping.GrafanaTeamID
		if teamID == 0 {
			id, found, err := s.grafana.SearchTeam(org.GrafanaOrgID, mapping.GrafanaTeamName)
			if err != nil {
				log.Printf("sync: search team %q failed: %v", mapping.GrafanaTeamName, err)
			} else if found {
				teamID = id
			}
		}
		if teamID == 0 {
			actions = append(actions, store.PlanAction{
				ActionType:    "create_team",
				OrgID:         org.ID,
				GrafanaOrgID:  org.GrafanaOrgID,
				TeamName:      mapping.GrafanaTeamName,
				ExternalGroupID: mapping.ExternalGroupID,
			})
		}

		members, err := s.entra.ListGroupMembers(mapping.ExternalGroupID)
		if err != nil {
			log.Printf("sync: list group members %s failed: %v", mapping.ExternalGroupID, err)
			continue
		}

		want := make(map[string]entra.Member)
		for _, member := range members {
			email := strings.TrimSpace(strings.ToLower(pickEmail(member)))
			if email == "" {
				continue
			}
			want[email] = member
		}

		have := make(map[string]grafana.User)
		if teamID != 0 {
			teamMembers, err := s.grafana.ListTeamMembers(teamID)
			if err != nil {
				log.Printf("sync: list team members %d failed: %v", teamID, err)
				continue
			}
			for _, tm := range teamMembers {
				email := strings.TrimSpace(strings.ToLower(tm.Email))
				if email != "" {
					have[email] = tm
				}
			}
		}

		role := mapping.RoleOverride
		if role == "" {
			if org.DefaultRole != "" {
				role = org.DefaultRole
			} else {
				role = s.defaultUserRole
			}
		}

		for email, member := range want {
			user, ok := userCache[email]
			if !ok {
				foundUser, found, err := s.grafana.LookupUser(email)
				if err != nil {
					log.Printf("sync: lookup user %s failed: %v", email, err)
					continue
				}
				if found {
					user = foundUser
				}
				userCache[email] = user
			}

			if user == nil {
				if !s.allowCreateUsers {
					actions = append(actions, store.PlanAction{
						ActionType:    "blocked_create_user",
						OrgID:         org.ID,
						GrafanaOrgID:  org.GrafanaOrgID,
						TeamID:        teamID,
						TeamName:      mapping.GrafanaTeamName,
						Email:         email,
						DisplayName:   member.DisplayName,
						Role:          role,
						ExternalGroupID: mapping.ExternalGroupID,
						Note:          "user not found and creation disabled",
					})
					continue
				}
				name := member.DisplayName
				if name == "" {
					name = email
				}
				actions = append(actions, store.PlanAction{
					ActionType:    "create_user",
					OrgID:         org.ID,
					GrafanaOrgID:  org.GrafanaOrgID,
					TeamID:        teamID,
					TeamName:      mapping.GrafanaTeamName,
					Email:         email,
					DisplayName:   name,
					Role:          role,
					ExternalGroupID: mapping.ExternalGroupID,
				})
			}

			if roleByOrgEmail[org.ID] == nil {
				roleByOrgEmail[org.ID] = map[string]string{}
			}
			roleByOrgEmail[org.ID][email] = maxRole(roleByOrgEmail[org.ID][email], role)

			if _, inTeam := have[email]; !inTeam {
				actions = append(actions, store.PlanAction{
					ActionType:    "add_user_to_team",
					OrgID:         org.ID,
					GrafanaOrgID:  org.GrafanaOrgID,
					TeamID:        teamID,
					TeamName:      mapping.GrafanaTeamName,
					UserID:        userID(user),
					Email:         email,
					Role:          role,
					ExternalGroupID: mapping.ExternalGroupID,
				})
			}
		}

		if s.allowRemoveUsers {
			for email, user := range have {
				if _, ok := want[email]; ok {
					continue
				}
				actions = append(actions, store.PlanAction{
					ActionType:    "remove_user_from_team",
					OrgID:         org.ID,
					GrafanaOrgID:  org.GrafanaOrgID,
					TeamID:        teamID,
					TeamName:      mapping.GrafanaTeamName,
					UserID:        user.ID,
					Email:         email,
					ExternalGroupID: mapping.ExternalGroupID,
				})
			}
		}
	}

	for orgID, roleMap := range roleByOrgEmail {
		org := orgByID[orgID]
		for email, role := range roleMap {
			user := userCache[email]
			actions = append(actions, store.PlanAction{
				ActionType:   "add_user_to_org",
				OrgID:        orgID,
				GrafanaOrgID: org.GrafanaOrgID,
				UserID:       userID(user),
				Email:        email,
				Role:         role,
			})
			actions = append(actions, store.PlanAction{
				ActionType:   "update_user_role",
				OrgID:        orgID,
				GrafanaOrgID: org.GrafanaOrgID,
				UserID:       userID(user),
				Email:        email,
				Role:         role,
			})
		}
	}

	plan := &store.Plan{
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Status:    "planned",
		Actions:   actions,
	}
	return plan, nil
}

func (s *Syncer) finish(start time.Time, err error) error {
	elapsed := time.Since(start)
	msg := "ok"
	if err != nil {
		msg = err.Error()
		log.Printf("sync: failed after %s: %v", elapsed.Round(time.Millisecond), err)
	} else {
		log.Printf("sync: completed in %s", elapsed.Round(time.Millisecond))
	}
	s.mu.Lock()
	s.lastRun = time.Now()
	s.lastMessage = msg
	s.mu.Unlock()
	return err
}

func pickEmail(member entra.Member) string {
	if member.Mail != "" {
		return member.Mail
	}
	return member.UPN
}

func randomPassword() string {
	buf := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return fmt.Sprintf("temp-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("temp-%x", buf)
}

func userID(user *grafana.User) int64 {
	if user == nil {
		return 0
	}
	return user.ID
}

func maxRole(current, candidate string) string {
	order := map[string]int{"Viewer": 1, "Editor": 2, "Admin": 3}
	if order[candidate] > order[current] {
		return candidate
	}
	if current == "" {
		return candidate
	}
	return current
}

func sortActions(actions []store.PlanAction) {
	order := map[string]int{
		"create_team":          1,
		"create_user":          2,
		"add_user_to_org":      3,
		"update_user_role":     4,
		"add_user_to_team":     5,
		"remove_user_from_team": 6,
	}
	sort.SliceStable(actions, func(i, j int) bool {
		return order[actions[i].ActionType] < order[actions[j].ActionType]
	})
}

func teamKey(orgID int64, teamName string) string {
	return fmt.Sprintf("%d:%s", orgID, strings.ToLower(teamName))
}
