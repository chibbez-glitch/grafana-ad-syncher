package web

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"grafana-ad-syncher/internal/entra"
	"grafana-ad-syncher/internal/grafana"
	"grafana-ad-syncher/internal/store"
	syncer "grafana-ad-syncher/internal/sync"
)

type Server struct {
	store   *store.Store
	syncer  *syncer.Syncer
	grafana *grafana.Client
	entra   *entra.Client
	tmpl    *template.Template
}

type grafanaTeamView struct {
	OrgID        int64
	OrgName      string
	TeamID       int64
	TeamName     string
	MappingInfo  string
	MappingState string
}

type grafanaUserView struct {
	ID    int64
	Login string
	Email string
	Name  string
}

type entraGroupView struct {
	ID           string
	DisplayName  string
	Mail         string
	SecurityType string
	MappingInfo  string
	MappingState string
}

type entraUserView struct {
	ID          string
	DisplayName string
	Mail        string
	UPN         string
	Enabled     bool
}

type pageData struct {
	Orgs             []store.Org
	Mappings         []store.Mapping
	GrafanaTeams     []grafanaTeamView
	GrafanaTeamsErr  string
	GrafanaUsers     []grafanaUserView
	GrafanaUsersErr  string
	EntraGroups      []entraGroupView
	EntraGroupsErr   string
	EntraUsers       []entraUserView
	EntraUsersErr    string
	LastRun          string
	LastStatus       string
	Plan             *store.Plan
}

func New(store *store.Store, syncer *syncer.Syncer, grafanaClient *grafana.Client, entraClient *entra.Client, templateDir string) (*Server, error) {
	tmpl, err := template.ParseFiles(
		filepath.Join(templateDir, "layout.html"),
		filepath.Join(templateDir, "index.html"),
	)
	if err != nil {
		return nil, err
	}
	return &Server{
		store:   store,
		syncer:  syncer,
		grafana: grafanaClient,
		entra:   entraClient,
		tmpl:    tmpl,
	}, nil
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/orgs", s.handleCreateOrg)
	mux.HandleFunc("/orgs/delete", s.handleDeleteOrg)
	mux.HandleFunc("/mappings", s.handleCreateMapping)
	mux.HandleFunc("/mappings/delete", s.handleDeleteMapping)
	mux.HandleFunc("/sync/preview", s.handlePreview)
	mux.HandleFunc("/sync/run", s.handleRun)
	mux.HandleFunc("/sync/apply", s.handleApply)
	mux.HandleFunc("/sync/clear", s.handleClearPlan)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	orgs, err := s.store.ListOrgs()
	if err != nil {
		http.Error(w, "failed to load orgs", http.StatusInternalServerError)
		return
	}
	mappings, err := s.store.ListMappings()
	if err != nil {
		http.Error(w, "failed to load mappings", http.StatusInternalServerError)
		return
	}
	plan, err := s.store.LatestPlan()
	if err != nil {
		http.Error(w, "failed to load plan", http.StatusInternalServerError)
		return
	}
	grafanaTeams, grafanaTeamsErr := s.loadGrafanaTeams(orgs, mappings)
	grafanaUsers, grafanaUsersErr := s.loadGrafanaUsers()
	entraGroups, entraGroupsErr := s.loadEntraGroups(orgs, mappings)
	entraUsers, entraUsersErr := s.loadEntraUsers()
	lastRun, lastStatus := s.syncer.LastRun()
	data := pageData{
		Orgs:            orgs,
		Mappings:        mappings,
		GrafanaTeams:    grafanaTeams,
		GrafanaTeamsErr: grafanaTeamsErr,
		GrafanaUsers:    grafanaUsers,
		GrafanaUsersErr: grafanaUsersErr,
		EntraGroups:     entraGroups,
		EntraGroupsErr:  entraGroupsErr,
		EntraUsers:      entraUsers,
		EntraUsersErr:   entraUsersErr,
		LastRun:         formatTime(lastRun),
		LastStatus:      lastStatus,
		Plan:            plan,
	}
	if err := s.tmpl.ExecuteTemplate(w, "layout.html", data); err != nil {
		log.Printf("render error: %v", err)
	}
}

func (s *Server) handleCreateOrg(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	orgID, _ := strconv.ParseInt(r.FormValue("grafana_org_id"), 10, 64)
	name := r.FormValue("name")
	defaultRole := r.FormValue("default_role")
	if defaultRole == "" {
		defaultRole = "Viewer"
	}
	_, err := s.store.CreateOrg(store.Org{GrafanaOrgID: orgID, Name: name, DefaultRole: defaultRole})
	if err != nil {
		http.Error(w, "failed to create org", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleDeleteOrg(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err := s.store.DeleteOrg(id); err != nil {
		http.Error(w, "failed to delete org", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleCreateMapping(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	orgID, _ := strconv.ParseInt(r.FormValue("org_id"), 10, 64)
	teamName := r.FormValue("grafana_team_name")
	externalGroupID := r.FormValue("external_group_id")
	externalGroupName := r.FormValue("external_group_name")
	roleOverride := r.FormValue("role_override")
	_, err := s.store.CreateMapping(store.Mapping{
		OrgID:             orgID,
		GrafanaTeamName:   teamName,
		ExternalGroupID:   externalGroupID,
		ExternalGroupName: externalGroupName,
		RoleOverride:      roleOverride,
	})
	if err != nil {
		http.Error(w, "failed to create mapping", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleDeleteMapping(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err := s.store.DeleteMapping(id); err != nil {
		http.Error(w, "failed to delete mapping", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	plan, err := s.syncer.BuildPlan()
	if err != nil {
		http.Error(w, "failed to build plan", http.StatusInternalServerError)
		return
	}
	if _, err := s.store.ReplacePlan(*plan); err != nil {
		http.Error(w, "failed to store plan", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	plan, err := s.syncer.BuildPlan()
	if err != nil {
		http.Error(w, "failed to build plan", http.StatusInternalServerError)
		return
	}
	planID, err := s.store.ReplacePlan(*plan)
	if err != nil {
		http.Error(w, "failed to store plan", http.StatusInternalServerError)
		return
	}
	if err := s.store.UpdatePlanStatus(planID, "applying"); err != nil {
		log.Printf("plan status update failed: %v", err)
	}
	err = s.syncer.ApplyPlan(plan.Actions)
	s.syncer.RecordRun(err)
	if err != nil {
		_ = s.store.UpdatePlanStatus(planID, "failed")
		http.Error(w, "apply failed", http.StatusInternalServerError)
		return
	}
	_ = s.store.UpdatePlanStatus(planID, "applied")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	plan, err := s.store.LatestPlan()
	if err != nil {
		http.Error(w, "failed to load plan", http.StatusInternalServerError)
		return
	}
	if plan == nil {
		http.Error(w, "no plan available", http.StatusBadRequest)
		return
	}
	if err := s.store.UpdatePlanStatus(plan.ID, "applying"); err != nil {
		log.Printf("plan status update failed: %v", err)
	}
	err = s.syncer.ApplyPlan(plan.Actions)
	s.syncer.RecordRun(err)
	if err != nil {
		_ = s.store.UpdatePlanStatus(plan.ID, "failed")
		http.Error(w, "apply failed", http.StatusInternalServerError)
		return
	}
	_ = s.store.UpdatePlanStatus(plan.ID, "applied")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleClearPlan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.store.ClearPlan(); err != nil {
		http.Error(w, "failed to clear plan", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Format(time.RFC3339)
}

func (s *Server) loadGrafanaTeams(orgs []store.Org, mappings []store.Mapping) ([]grafanaTeamView, string) {
	if s.grafana == nil {
		return nil, "grafana client not configured"
	}
	byName := map[string][]store.Mapping{}
	byID := map[string][]store.Mapping{}
	for _, m := range mappings {
		if m.GrafanaTeamName != "" {
			key := fmt.Sprintf("%d:%s", m.OrgID, strings.ToLower(m.GrafanaTeamName))
			byName[key] = append(byName[key], m)
		}
		if m.GrafanaTeamID > 0 {
			key := fmt.Sprintf("%d:%d", m.OrgID, m.GrafanaTeamID)
			byID[key] = append(byID[key], m)
		}
	}

	var views []grafanaTeamView
	var errs []string
	for _, org := range orgs {
		teams, err := s.grafana.ListTeams(org.GrafanaOrgID)
		if err != nil {
			errs = append(errs, fmt.Sprintf("org %d: %v", org.GrafanaOrgID, err))
			continue
		}
		for _, team := range teams {
			var mapped []store.Mapping
			if team.ID > 0 {
				mapped = byID[fmt.Sprintf("%d:%d", org.ID, team.ID)]
			}
			if len(mapped) == 0 {
				mapped = byName[fmt.Sprintf("%d:%s", org.ID, strings.ToLower(team.Name))]
			}
			info := mappingGroupsSummary(mapped)
			state := "unmapped"
			if info != "" {
				state = "mapped"
			}
			views = append(views, grafanaTeamView{
				OrgID:        org.GrafanaOrgID,
				OrgName:      org.Name,
				TeamID:       team.ID,
				TeamName:     team.Name,
				MappingInfo:  info,
				MappingState: state,
			})
		}
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].OrgID == views[j].OrgID {
			return strings.ToLower(views[i].TeamName) < strings.ToLower(views[j].TeamName)
		}
		return views[i].OrgID < views[j].OrgID
	})
	return views, strings.Join(errs, "; ")
}

func (s *Server) loadGrafanaUsers() ([]grafanaUserView, string) {
	if s.grafana == nil {
		return nil, "grafana client not configured"
	}
	users, err := s.grafana.ListAdminUsers()
	if err != nil {
		return nil, err.Error()
	}
	views := make([]grafanaUserView, 0, len(users))
	for _, user := range users {
		views = append(views, grafanaUserView{
			ID:    user.ID,
			Login: user.Login,
			Email: user.Email,
			Name:  user.Name,
		})
	}
	sort.Slice(views, func(i, j int) bool {
		return strings.ToLower(views[i].Login) < strings.ToLower(views[j].Login)
	})
	return views, ""
}

func (s *Server) loadEntraGroups(orgs []store.Org, mappings []store.Mapping) ([]entraGroupView, string) {
	if s.entra == nil {
		return nil, "entra client not configured"
	}
	groups, err := s.entra.ListGroups()
	if err != nil {
		return nil, err.Error()
	}
	orgNames := map[int64]string{}
	for _, org := range orgs {
		orgNames[org.ID] = org.Name
	}
	byGroup := map[string][]store.Mapping{}
	for _, m := range mappings {
		if m.ExternalGroupID == "" {
			continue
		}
		byGroup[m.ExternalGroupID] = append(byGroup[m.ExternalGroupID], m)
	}
	views := make([]entraGroupView, 0, len(groups))
	for _, group := range groups {
		mapped := byGroup[group.ID]
		info := mappingTeamsSummary(mapped, orgNames)
		state := "unmapped"
		if info != "" {
			state = "mapped"
		}
		securityType := "security"
		if group.MailEnabled {
			securityType = "m365"
		} else if !group.SecurityEnabled {
			securityType = "distribution"
		}
		views = append(views, entraGroupView{
			ID:           group.ID,
			DisplayName:  group.DisplayName,
			Mail:         group.Mail,
			SecurityType: securityType,
			MappingInfo:  info,
			MappingState: state,
		})
	}
	sort.Slice(views, func(i, j int) bool {
		return strings.ToLower(views[i].DisplayName) < strings.ToLower(views[j].DisplayName)
	})
	return views, ""
}

func (s *Server) loadEntraUsers() ([]entraUserView, string) {
	if s.entra == nil {
		return nil, "entra client not configured"
	}
	users, err := s.entra.ListUsers()
	if err != nil {
		return nil, err.Error()
	}
	views := make([]entraUserView, 0, len(users))
	for _, user := range users {
		views = append(views, entraUserView{
			ID:          user.ID,
			DisplayName: user.DisplayName,
			Mail:        user.Mail,
			UPN:         user.UPN,
			Enabled:     user.AccountEnabled,
		})
	}
	sort.Slice(views, func(i, j int) bool {
		return strings.ToLower(views[i].DisplayName) < strings.ToLower(views[j].DisplayName)
	})
	return views, ""
}

func mappingGroupsSummary(mappings []store.Mapping) string {
	if len(mappings) == 0 {
		return ""
	}
	values := make([]string, 0, len(mappings))
	for _, mapping := range mappings {
		label := strings.TrimSpace(mapping.ExternalGroupName)
		if label == "" {
			label = mapping.ExternalGroupID
		} else if mapping.ExternalGroupID != "" {
			label = fmt.Sprintf("%s (%s)", label, mapping.ExternalGroupID)
		}
		values = append(values, label)
	}
	sort.Strings(values)
	return strings.Join(values, ", ")
}

func mappingTeamsSummary(mappings []store.Mapping, orgNames map[int64]string) string {
	if len(mappings) == 0 {
		return ""
	}
	values := make([]string, 0, len(mappings))
	for _, mapping := range mappings {
		orgLabel := orgNames[mapping.OrgID]
		if orgLabel == "" {
			orgLabel = fmt.Sprintf("org %d", mapping.OrgID)
		}
		teamLabel := mapping.GrafanaTeamName
		if teamLabel == "" {
			teamLabel = fmt.Sprintf("team %d", mapping.GrafanaTeamID)
		}
		values = append(values, fmt.Sprintf("%s: %s", orgLabel, teamLabel))
	}
	sort.Strings(values)
	return strings.Join(values, ", ")
}
