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
	TeamRole        string
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
			if err := s.store.RecordSyncAction(action, time.Now()); err != nil {
				log.Printf("sync: record action failed: %v", err)
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
			if err := s.store.RecordSyncAction(action, time.Now()); err != nil {
				log.Printf("sync: record action failed: %v", err)
			}
		case "add_user_to_org":
			if err := s.grafana.AddUserToOrg(action.GrafanaOrgID, email, action.Role); err != nil {
				return err
			}
			if err := s.store.RecordSyncAction(action, time.Now()); err != nil {
				log.Printf("sync: record action failed: %v", err)
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
					if isExternallySyncedUserErr(err) {
						log.Printf("sync: skip update role for externally synced user %s: %v", email, err)
						continue
					}
					return err
				}
			}
			if err := s.store.RecordSyncAction(action, time.Now()); err != nil {
				log.Printf("sync: record action failed: %v", err)
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
				if err := s.grafana.AddUserToTeam(teamID, id, action.TeamRole); err != nil {
					return err
				}
			}
			if err := s.store.RecordSyncAction(action, time.Now()); err != nil {
				log.Printf("sync: record action failed: %v", err)
			}
		case "update_team_role":
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
				if err := s.grafana.UpdateTeamMemberRole(teamID, id, action.TeamRole); err != nil {
					return err
				}
			}
			if err := s.store.RecordSyncAction(action, time.Now()); err != nil {
				log.Printf("sync: record action failed: %v", err)
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
			if err := s.store.RecordSyncAction(action, time.Now()); err != nil {
				log.Printf("sync: record action failed: %v", err)
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
	orgNameByID := make(map[int64]string, len(orgs))
	for _, org := range orgs {
		orgByID[org.ID] = org
		orgNameByID[org.ID] = org.Name
	}

	mappings, err := s.store.ListMappings()
	if err != nil {
		return nil, fmt.Errorf("list mappings: %w", err)
	}

	var actions []store.PlanAction
	userCache := map[string]*grafana.User{}
	roleByOrgEmail := map[int64]map[string]string{}
	roleSourceByOrgEmail := map[int64]map[string]string{}
	addedTeamUsers := map[string]int{}
	teamRoleByTeamEmail := map[string]map[string]string{}
	updatedTeamRoles := map[string]struct{}{}

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
				TeamRole:      normalizeTeamRole(mapping.TeamRole),
				ExternalGroupID: mapping.ExternalGroupID,
				Note:          mappingNote(orgNameByID[org.ID], mapping),
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
			key := teamKey(org.ID, mapping.GrafanaTeamName)
			if teamRoleByTeamEmail[key] == nil {
				teamRoleByTeamEmail[key] = map[string]string{}
			}
			current := teamRoleByTeamEmail[key][email]
			teamRoleByTeamEmail[key][email] = maxTeamRole(current, normalizeTeamRole(mapping.TeamRole))
		}

		have := make(map[string]grafana.TeamMember)
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
		roleSource := ""
		if role == "" {
			if org.DefaultRole != "" {
				role = org.DefaultRole
				roleSource = fmt.Sprintf("org default role: %s", org.DefaultRole)
			} else {
				role = s.defaultUserRole
				roleSource = fmt.Sprintf("service default role: %s", s.defaultUserRole)
			}
		} else {
			roleSource = fmt.Sprintf("mapping role override: %s", role)
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
						Note:          appendNote("user not found and creation disabled", mappingNote(orgNameByID[org.ID], mapping)),
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
					Note:          mappingNote(orgNameByID[org.ID], mapping),
				})
			}

			if roleByOrgEmail[org.ID] == nil {
				roleByOrgEmail[org.ID] = map[string]string{}
			}
			if roleSourceByOrgEmail[org.ID] == nil {
				roleSourceByOrgEmail[org.ID] = map[string]string{}
			}
			current := roleByOrgEmail[org.ID][email]
			next := maxRole(current, role)
			roleByOrgEmail[org.ID][email] = next
			if next != current {
				roleSourceByOrgEmail[org.ID][email] = fmt.Sprintf("%s; %s", roleSource, mappingNote(orgNameByID[org.ID], mapping))
			}

			if _, inTeam := have[email]; !inTeam {
				key := teamKey(org.ID, mapping.GrafanaTeamName)
				teamUserKey := key + ":" + email
				teamRole := teamRoleByTeamEmail[key][email]
				if teamRole == "" {
					teamRole = "member"
				}
				if idx, exists := addedTeamUsers[teamUserKey]; exists {
					if maxTeamRole(actions[idx].TeamRole, teamRole) != actions[idx].TeamRole {
						actions[idx].TeamRole = teamRole
					}
				} else {
					actions = append(actions, store.PlanAction{
						ActionType:    "add_user_to_team",
						OrgID:         org.ID,
						GrafanaOrgID:  org.GrafanaOrgID,
						TeamID:        teamID,
						TeamName:      mapping.GrafanaTeamName,
						TeamRole:      teamRole,
						UserID:        userID(user),
						Email:         email,
						Role:          role,
						ExternalGroupID: mapping.ExternalGroupID,
						Note:          mappingNote(orgNameByID[org.ID], mapping),
					})
					addedTeamUsers[teamUserKey] = len(actions) - 1
				}
			} else {
				teamRole := teamRoleByTeamEmail[teamKey(org.ID, mapping.GrafanaTeamName)][email]
				if teamRole == "admin" {
					updateKey := teamKey(org.ID, mapping.GrafanaTeamName) + ":" + email
					if _, exists := updatedTeamRoles[updateKey]; !exists {
						actions = append(actions, store.PlanAction{
							ActionType:    "update_team_role",
							OrgID:         org.ID,
							GrafanaOrgID:  org.GrafanaOrgID,
							TeamID:        teamID,
							TeamName:      mapping.GrafanaTeamName,
							TeamRole:      teamRole,
							UserID:        userID(user),
							Email:         email,
							ExternalGroupID: mapping.ExternalGroupID,
							Note:          mappingNote(orgNameByID[org.ID], mapping),
						})
						updatedTeamRoles[updateKey] = struct{}{}
					}
				}
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
					Note:          mappingNote(orgNameByID[org.ID], mapping),
				})
			}
		}
	}

	orgUsersByOrgEmail := map[int64]map[string]grafana.OrgUser{}
	for _, org := range orgs {
		users, err := s.grafana.ListOrgUsers(org.GrafanaOrgID)
		if err != nil {
			log.Printf("sync: list org users %d failed: %v", org.GrafanaOrgID, err)
			continue
		}
		if orgUsersByOrgEmail[org.ID] == nil {
			orgUsersByOrgEmail[org.ID] = map[string]grafana.OrgUser{}
		}
		for _, user := range users {
			email := strings.ToLower(strings.TrimSpace(user.Email))
			if email == "" {
				continue
			}
			orgUsersByOrgEmail[org.ID][email] = user
		}
	}

	for orgID, roleMap := range roleByOrgEmail {
		org := orgByID[orgID]
		orgUsers := orgUsersByOrgEmail[orgID]
		for email, role := range roleMap {
			key := strings.ToLower(strings.TrimSpace(email))
			var existing grafana.OrgUser
			found := false
			if orgUsers != nil {
				existing, found = orgUsers[key]
			}
			user := userCache[email]
			if !found {
				note := roleSourceByOrgEmail[orgID][email]
				if orgUsers == nil {
					note = appendNote(note, "org user lookup failed")
				}
				actions = append(actions, store.PlanAction{
					ActionType:   "add_user_to_org",
					OrgID:        orgID,
					GrafanaOrgID: org.GrafanaOrgID,
					UserID:       userID(user),
					Email:        email,
					Role:         role,
					Note:         note,
				})
				continue
			}
			if !strings.EqualFold(existing.Role, role) {
				userIDValue := userID(user)
				if userIDValue == 0 {
					userIDValue = existing.ID
				}
				actions = append(actions, store.PlanAction{
					ActionType:   "update_user_role",
					OrgID:        orgID,
					GrafanaOrgID: org.GrafanaOrgID,
					UserID:       userIDValue,
					Email:        email,
					Role:         role,
					Note:         appendNote(roleSourceByOrgEmail[orgID][email], fmt.Sprintf("current role: %s", existing.Role)),
				})
			}
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
	return member.Mail
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

func normalizeTeamRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "admin":
		return "admin"
	default:
		return "member"
	}
}

func maxTeamRole(current, candidate string) string {
	if strings.ToLower(candidate) == "admin" {
		return "admin"
	}
	if current == "" {
		return "member"
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
		"update_team_role":     6,
		"remove_user_from_team": 7,
	}
	sort.SliceStable(actions, func(i, j int) bool {
		return order[actions[i].ActionType] < order[actions[j].ActionType]
	})
}

func isExternallySyncedUserErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "org.externallySynced")
}

func teamKey(orgID int64, teamName string) string {
	return fmt.Sprintf("%d:%s", orgID, strings.ToLower(teamName))
}

func mappingNote(orgName string, mapping store.Mapping) string {
	orgLabel := orgName
	if strings.TrimSpace(orgLabel) == "" {
		orgLabel = fmt.Sprintf("org %d", mapping.OrgID)
	}
	groupLabel := strings.TrimSpace(mapping.ExternalGroupName)
	if groupLabel == "" {
		groupLabel = mapping.ExternalGroupID
	} else if mapping.ExternalGroupID != "" {
		groupLabel = fmt.Sprintf("%s (%s)", groupLabel, mapping.ExternalGroupID)
	}
	teamLabel := mapping.GrafanaTeamName
	if strings.TrimSpace(teamLabel) == "" {
		teamLabel = fmt.Sprintf("team %d", mapping.GrafanaTeamID)
	}
	return fmt.Sprintf("mapping: %s/%s <- %s", orgLabel, teamLabel, groupLabel)
}

func appendNote(base, addition string) string {
	base = strings.TrimSpace(base)
	addition = strings.TrimSpace(addition)
	if addition == "" {
		return base
	}
	if base == "" {
		return addition
	}
	return base + "; " + addition
}
