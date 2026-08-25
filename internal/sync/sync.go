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
	planIssues  []Issue
	applyIssues []Issue
}

// Issue is a problem hit during a sync that does not abort the run: a user that
// could not be resolved, a group that could not be read, an action that was
// skipped. These used to only reach the log, where nobody saw them, so the UI
// showed an empty plan and no explanation.
type Issue struct {
	At       time.Time
	Phase    string // "plan" or "apply"
	Severity string // "error" or "warning"
	Scope    string // team or mapping the issue belongs to
	Email    string
	Message  string
}

const (
	PhasePlan  = "plan"
	PhaseApply = "apply"

	SeverityError   = "error"
	SeverityWarning = "warning"
)

// Issues returns the problems recorded by the most recent plan and apply,
// newest phase first.
func (s *Syncer) Issues() []Issue {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Issue, 0, len(s.applyIssues)+len(s.planIssues))
	out = append(out, s.applyIssues...)
	out = append(out, s.planIssues...)
	return out
}

func (s *Syncer) resetIssues(phase string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if phase == PhaseApply {
		s.applyIssues = nil
		return
	}
	s.planIssues = nil
	s.applyIssues = nil
}

func (s *Syncer) addIssue(phase, severity, scope, email, format string, args ...any) {
	issue := Issue{
		At:       time.Now().UTC(),
		Phase:    phase,
		Severity: severity,
		Scope:    scope,
		Email:    email,
		Message:  fmt.Sprintf(format, args...),
	}
	log.Printf("sync: %s %s: %s (%s%s)", phase, severity, issue.Message, scope, emailSuffix(email))

	s.mu.Lock()
	defer s.mu.Unlock()
	if phase == PhaseApply {
		s.applyIssues = append(s.applyIssues, issue)
		return
	}
	s.planIssues = append(s.planIssues, issue)
}

func emailSuffix(email string) string {
	if email == "" {
		return ""
	}
	return " / " + email
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
	s.resetIssues(PhaseApply)
	sortActions(actions)
	userIDs := map[string]int64{}
	teamIDs := map[string]int64{}
	failed := 0

	// fail records a failed action and carries on. Aborting on the first error
	// used to swallow every later action, and since create_user sorts ahead of
	// add_user_to_team, one uncreatable user emptied the whole run.
	fail := func(action store.PlanAction, format string, args ...any) {
		failed++
		s.addIssue(PhaseApply, SeverityError, actionScope(action), action.Email, format, args...)
	}

	for _, action := range actions {
		email := action.Email
		switch action.ActionType {
		case "create_team":
			teamID, err := s.grafana.EnsureTeam(action.GrafanaOrgID, action.TeamName)
			if err != nil {
				fail(action, "create team %q failed: %v", action.TeamName, err)
				continue
			}
			teamIDs[teamKey(action.OrgID, action.TeamName)] = teamID
			if err := s.store.UpdateMappingTeamIDForName(action.OrgID, action.TeamName, teamID); err != nil {
				s.addIssue(PhaseApply, SeverityWarning, actionScope(action), "",
					"team %q created but storing its id failed: %v", action.TeamName, err)
			}
			s.recordAction(action)
		case "create_user":
			name := action.DisplayName
			if name == "" {
				name = email
			}
			created, err := s.grafana.CreateUser(email, email, name, randomPassword())
			if err != nil {
				fail(action, "create user failed: %v%s", err, createUserHint(err))
				continue
			}
			userIDs[email] = created.ID
			s.grafana.InvalidateOrgUsers(action.GrafanaOrgID)
			s.recordAction(action)
		case "add_user_to_org":
			if err := s.grafana.AddUserToOrg(action.GrafanaOrgID, email, action.Role); err != nil {
				fail(action, "add user to org %d as %s failed: %v", action.GrafanaOrgID, action.Role, err)
				continue
			}
			s.grafana.InvalidateOrgUsers(action.GrafanaOrgID)
			s.recordAction(action)
		case "update_user_role":
			id, err := s.resolveActionUser(action, userIDs)
			if err != nil {
				fail(action, "Grafana user lookup failed: %v", err)
				continue
			}
			if id == 0 {
				s.addIssue(PhaseApply, SeverityWarning, actionScope(action), email,
					"org role not updated: no Grafana user found")
				continue
			}
			if err := s.grafana.UpdateUserRole(action.GrafanaOrgID, id, action.Role); err != nil {
				if isExternallySyncedUserErr(err) {
					s.addIssue(PhaseApply, SeverityWarning, actionScope(action), email,
						"org role not changed: Grafana manages this user externally")
					continue
				}
				fail(action, "update org role to %s failed: %v", action.Role, err)
				continue
			}
			s.recordAction(action)
		case "add_user_to_team":
			teamID := resolveActionTeam(action, teamIDs)
			if teamID == 0 {
				fail(action, "no Grafana team id known for %q", action.TeamName)
				continue
			}
			id, err := s.resolveActionUser(action, userIDs)
			if err != nil {
				fail(action, "Grafana user lookup failed: %v", err)
				continue
			}
			if id == 0 {
				s.addIssue(PhaseApply, SeverityWarning, actionScope(action), email,
					"not added to team %q: no Grafana user found in org %d", action.TeamName, action.GrafanaOrgID)
				continue
			}
			if err := s.grafana.AddUserToTeam(teamID, id, action.TeamRole); err != nil {
				fail(action, "add to team %q failed: %v", action.TeamName, err)
				continue
			}
			s.recordAction(action)
		case "update_team_role":
			teamID := resolveActionTeam(action, teamIDs)
			if teamID == 0 {
				fail(action, "no Grafana team id known for %q", action.TeamName)
				continue
			}
			id, err := s.resolveActionUser(action, userIDs)
			if err != nil {
				fail(action, "Grafana user lookup failed: %v", err)
				continue
			}
			if id == 0 {
				s.addIssue(PhaseApply, SeverityWarning, actionScope(action), email,
					"team role not updated: no Grafana user found")
				continue
			}
			if err := s.grafana.UpdateTeamMemberRole(teamID, id, action.TeamRole); err != nil {
				fail(action, "update team role in %q failed: %v", action.TeamName, err)
				continue
			}
			s.recordAction(action)
		case "remove_user_from_team":
			teamID := resolveActionTeam(action, teamIDs)
			if teamID == 0 {
				fail(action, "no Grafana team id known for %q", action.TeamName)
				continue
			}
			id, err := s.resolveActionUser(action, userIDs)
			if err != nil {
				fail(action, "Grafana user lookup failed: %v", err)
				continue
			}
			if id == 0 {
				s.addIssue(PhaseApply, SeverityWarning, actionScope(action), email,
					"not removed from team %q: no Grafana user found", action.TeamName)
				continue
			}
			if err := s.grafana.RemoveUserFromTeam(teamID, id); err != nil {
				fail(action, "remove from team %q failed: %v", action.TeamName, err)
				continue
			}
			s.recordAction(action)
		default:
			continue
		}
	}

	if failed > 0 {
		return fmt.Errorf("%d of %d actions failed, see sync issues", failed, len(actions))
	}
	return nil
}

func (s *Syncer) recordAction(action store.PlanAction) {
	if err := s.store.RecordSyncAction(action, time.Now()); err != nil {
		log.Printf("sync: record action failed: %v", err)
	}
}

func resolveActionTeam(action store.PlanAction, teamIDs map[string]int64) int64 {
	if action.TeamID != 0 {
		return action.TeamID
	}
	return teamIDs[teamKey(action.OrgID, action.TeamName)]
}

// resolveActionUser resolves an action's Grafana user id: the id the plan
// already carries, then one created earlier in this apply, then an org-scoped
// lookup.
func (s *Syncer) resolveActionUser(action store.PlanAction, userIDs map[string]int64) (int64, error) {
	if action.UserID != 0 {
		return action.UserID, nil
	}
	if id := userIDs[action.Email]; id != 0 {
		return id, nil
	}
	user, found, err := s.grafana.LookupUserInOrg(action.GrafanaOrgID, action.Email)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, nil
	}
	return user.ID, nil
}

// createUserHint explains the status Grafana returns for POST /api/admin/users
// when the caller is not a server admin — a service account token never is, and
// the bare 403/404 gives no clue why.
func createUserHint(err error) string {
	switch grafana.StatusOf(err) {
	case 401, 403, 404:
		return " — POST /api/admin/users requires Grafana server admin, which a service account token cannot hold." +
			" Either set ALLOW_CREATE_USERS=false and pre-create the user, or authenticate with admin credentials."
	default:
		return ""
	}
}

func actionScope(action store.PlanAction) string {
	if strings.TrimSpace(action.TeamName) != "" {
		return action.TeamName
	}
	return fmt.Sprintf("org %d", action.GrafanaOrgID)
}

func (s *Syncer) BuildPlan() (*store.Plan, error) {
	s.resetIssues(PhasePlan)
	// Plan against a fresh view of Grafana rather than a previous run's snapshot.
	s.grafana.ResetUserCache()

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
	// plannedCreate keeps one create per person across all mappings. The loop
	// below runs per mapping, and someone in two mapped groups used to collect
	// two identical create_user actions: the first succeeded, the second came
	// back 412 "already exists" and turned an otherwise clean apply into a
	// failure. Team and org membership still get their own action per mapping -
	// only the account creation is global.
	plannedCreate := map[string]struct{}{}
	roleByOrgEmail := map[int64]map[string]string{}
	roleSourceByOrgEmail := map[int64]map[string]string{}
	addedTeamUsers := map[string]int{}
	teamRoleByTeamEmail := map[string]map[string]string{}
	updatedTeamRoles := map[string]struct{}{}

	for _, mapping := range mappings {
		org, ok := orgByID[mapping.OrgID]
		if !ok {
			s.addIssue(PhasePlan, SeverityError, fmt.Sprintf("mapping %d", mapping.ID), "",
				"mapping references org %d, which does not exist", mapping.OrgID)
			continue
		}

		teamID := mapping.GrafanaTeamID
		if teamID == 0 {
			id, found, err := s.grafana.SearchTeam(org.GrafanaOrgID, mapping.GrafanaTeamName)
			if err != nil {
				s.addIssue(PhasePlan, SeverityWarning, mappingNote(orgNameByID[org.ID], mapping), "",
					"Grafana team search failed: %v", err)
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

		scope := mappingNote(orgNameByID[org.ID], mapping)

		members, err := s.entra.ListGroupMembers(mapping.ExternalGroupID)
		if err != nil {
			s.addIssue(PhasePlan, SeverityError, scope, "",
				"Entra group members could not be read, mapping skipped: %v", err)
			continue
		}

		want := make(map[string]entra.Member)
		for _, member := range members {
			email := strings.TrimSpace(strings.ToLower(pickEmail(member)))
			if email == "" {
				s.addIssue(PhasePlan, SeverityWarning, scope, member.UPN,
					"Entra member %q has neither mail nor userPrincipalName, skipped", member.DisplayName)
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

		// Index existing team members by both email and login: Grafana may hold
		// the UPN where Entra holds the mail, and a single key would then make an
		// existing member look absent — planning an add and a remove for one and
		// the same person.
		have := make(map[string]grafana.TeamMember)
		haveByID := make(map[int64]grafana.TeamMember)
		if teamID != 0 {
			teamMembers, err := s.grafana.ListTeamMembers(teamID)
			if err != nil {
				s.addIssue(PhasePlan, SeverityError, scope, "",
					"Grafana team members could not be read, mapping skipped: %v", err)
				continue
			}
			for _, tm := range teamMembers {
				haveByID[tm.ID] = tm
				for _, key := range []string{tm.Email, tm.Login} {
					key = strings.TrimSpace(strings.ToLower(key))
					if key != "" {
						have[key] = tm
					}
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
				foundUser, found, err := s.resolveMember(org.GrafanaOrgID, member)
				if err != nil {
					s.addIssue(PhasePlan, SeverityError, scope, email,
						"Grafana user lookup failed, user skipped: %v", err)
					continue
				}
				if found {
					user = foundUser
				}
				userCache[email] = user
			}

			if user == nil {
				_, accountedFor := plannedCreate[email]

				if !s.allowCreateUsers {
					// Genuinely skipped: no account, and we may not make one. Warn
					// once per person rather than once per mapping - the blocked
					// action below already names every affected mapping.
					if !accountedFor {
						plannedCreate[email] = struct{}{}
						s.addIssue(PhasePlan, SeverityWarning, scope, email,
							"no Grafana user matches mail %q or UPN %q in org %d, and user creation is disabled",
							member.Mail, member.UPN, org.GrafanaOrgID)
					}
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

				// Creation is allowed, so nothing is being skipped and this is not
				// a warning. The plan carries a visible "create user" row instead;
				// warning on it as well is what produced "49 warnings" for a plan
				// that was entirely correct, and warnings you learn to ignore are
				// how the original breakage stayed unnoticed for months.
				if !accountedFor {
					plannedCreate[email] = struct{}{}
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

			if _, inTeam := teamMemberFor(have, member); !inTeam {
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
			// Resolve who is wanted by user id first. Comparing e-mail keys alone
			// would mark a member for removal whose Grafana account is known under
			// its UPN while Entra lists its mail.
			wantedIDs := make(map[int64]struct{}, len(want))
			for _, member := range want {
				if tm, ok := teamMemberFor(have, member); ok {
					wantedIDs[tm.ID] = struct{}{}
				}
			}
			for _, user := range haveByID {
				if _, ok := wantedIDs[user.ID]; ok {
					continue
				}
				email := strings.TrimSpace(strings.ToLower(user.Email))
				if email == "" {
					email = strings.TrimSpace(strings.ToLower(user.Login))
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
			s.addIssue(PhasePlan, SeverityError, org.Name, "",
				"Grafana org users could not be read, org roles not planned: %v", err)
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

// resolveMember maps an Entra member onto a Grafana user, trying the mail first
// and then the UPN. The two routinely differ in Entra (Abdelmomen.Bouzarkouna@
// vs abouzarkouna@), and Grafana stores whichever one the login provider sent.
func (s *Syncer) resolveMember(grafanaOrgID int64, member entra.Member) (*grafana.User, bool, error) {
	for _, candidate := range loginCandidates(member) {
		user, found, err := s.grafana.LookupUserInOrg(grafanaOrgID, candidate)
		if err != nil {
			return nil, false, err
		}
		if found {
			return user, true, nil
		}
	}
	return nil, false, nil
}

// teamMemberFor finds an Entra member among a team's current members, matching
// on mail and UPN against both the Grafana email and login.
func teamMemberFor(have map[string]grafana.TeamMember, member entra.Member) (grafana.TeamMember, bool) {
	for _, candidate := range loginCandidates(member) {
		if tm, ok := have[candidate]; ok {
			return tm, true
		}
	}
	return grafana.TeamMember{}, false
}

// loginCandidates returns the identifiers to try against Grafana, mail first,
// lower-cased and de-duplicated.
func loginCandidates(member entra.Member) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, value := range []string{member.Mail, member.UPN} {
		value = strings.TrimSpace(strings.ToLower(value))
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

// pickEmail is the member's identity for planning and display. Entra leaves mail
// empty on plenty of accounts, so fall back to the UPN rather than dropping them.
func pickEmail(member entra.Member) string {
	if mail := strings.TrimSpace(member.Mail); mail != "" {
		return mail
	}
	return strings.TrimSpace(member.UPN)
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
