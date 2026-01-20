package web

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	cacheMu sync.RWMutex
	cache   externalCache
	refresh bool
}

type externalCache struct {
	grafanaTeams    []grafanaTeamView
	grafanaTeamsErr string
	grafanaUsers    []grafanaUserView
	grafanaUsersErr string
	entraGroups     []entraGroupView
	entraGroupsErr  string
	entraUsers      []entraUserView
	entraUsersErr   string
	refreshedAt     time.Time
}

type grafanaTeamView struct {
	OrgID        int64
	OrgName      string
	TeamID       int64
	TeamName     string
	MemberCount  int
	GroupIDsCSV  string
	MappingInfo  string
	MappingState string
}

type grafanaUserView struct {
	ID    int64
	Login string
	Email string
	Name  string
	Teams string
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
	Department  string
	Groups      string
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
	PlanGroups       []planTeamGroup
	LastRun          string
	LastStatus       string
	Plan             *store.Plan
	AutoSyncEnabled  bool
	CurrentPage      string
	ContentTemplate  string
}

type planActionView struct {
	ID         int64
	Type       string
	OrgID      int64
	Team       string
	Email      string
	Role       string
	TeamRole   string
	Note       string
	Class      string
	Selectable bool
}

type planTeamGroup struct {
	Title   string
	Actions []planActionView
}

func New(store *store.Store, syncer *syncer.Syncer, grafanaClient *grafana.Client, entraClient *entra.Client, templateDir string) (*Server, error) {
	tmpl, err := template.New("layout.html").Funcs(template.FuncMap{
		"actionClass":  actionClass,
		"actionLabel":  actionLabel,
		"isSelectable": isSelectableAction,
	}).ParseFiles(
		filepath.Join(templateDir, "layout.html"),
		filepath.Join(templateDir, "index.html"),
		filepath.Join(templateDir, "grafana.html"),
		filepath.Join(templateDir, "entra.html"),
	)
	if err != nil {
		return nil, err
	}
	server := &Server{
		store:   store,
		syncer:  syncer,
		grafana: grafanaClient,
		entra:   entraClient,
		tmpl:    tmpl,
	}
	go server.refreshLoop(30 * time.Second)
	return server, nil
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/grafana", s.handleGrafanaSettings)
	mux.HandleFunc("/entra", s.handleEntraSettings)
	mux.HandleFunc("/api/status", s.handleAPIStatus)
	mux.HandleFunc("/sync/fetch", s.handleFetch)
	mux.HandleFunc("/orgs", s.handleCreateOrg)
	mux.HandleFunc("/orgs/delete", s.handleDeleteOrg)
	mux.HandleFunc("/mappings", s.handleCreateMapping)
	mux.HandleFunc("/mappings/delete", s.handleDeleteMapping)
	mux.HandleFunc("/mappings/purge", s.handlePurgeMappings)
	mux.HandleFunc("/entra/group/members", s.handleEntraGroupMembers)
	mux.HandleFunc("/settings/auto-sync", s.handleAutoSync)
	mux.HandleFunc("/sync/preview", s.handlePreview)
	mux.HandleFunc("/sync/run", s.handleRun)
	mux.HandleFunc("/sync/apply", s.handleApply)
	mux.HandleFunc("/sync/apply-selected", s.handleApplySelected)
	mux.HandleFunc("/sync/clear", s.handleClearPlan)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	start := time.Now()

	data, err := s.buildPageData()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data.CurrentPage = "home"
	data.ContentTemplate = "content-index"
	if err := s.tmpl.ExecuteTemplate(w, "layout.html", data); err != nil {
		log.Printf("render error: %v", err)
	}
	log.Printf("ui: index rendered in %s", time.Since(start).Round(time.Millisecond))
}

func (s *Server) buildPageData() (pageData, error) {
	orgs, err := s.store.ListOrgs()
	if err != nil {
		return pageData{}, fmt.Errorf("failed to load orgs")
	}
	mappings, err := s.store.ListMappings()
	if err != nil {
		return pageData{}, fmt.Errorf("failed to load mappings")
	}
	plan, err := s.store.LatestPlan()
	if err != nil {
		return pageData{}, fmt.Errorf("failed to load plan")
	}
	grafanaTeams, grafanaTeamsErr, grafanaUsers, grafanaUsersErr, entraGroups, entraGroupsErr, entraUsers, entraUsersErr := s.getExternalData(orgs, mappings)
	var planGroups []planTeamGroup
	if plan != nil {
		planGroups = buildPlanGroups(plan.Actions)
	}
	lastRun, lastStatus := s.syncer.LastRun()
	autoSyncEnabled := true
	if enabled, err := s.store.AutoSyncEnabled(); err != nil {
		log.Printf("ui: auto sync state load failed: %v", err)
	} else {
		autoSyncEnabled = enabled
	}
	return pageData{
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
		PlanGroups:      planGroups,
		LastRun:         formatTime(lastRun),
		LastStatus:      lastStatus,
		Plan:            plan,
		AutoSyncEnabled: autoSyncEnabled,
	}, nil
}

func (s *Server) refreshLoop(interval time.Duration) {
	for {
		s.refreshExternalData()
		time.Sleep(interval)
	}
}

func (s *Server) refreshExternalData() {
	s.cacheMu.Lock()
	if s.refresh {
		s.cacheMu.Unlock()
		return
	}
	s.refresh = true
	s.cacheMu.Unlock()

	orgs, err := s.store.ListOrgs()
	if err != nil {
		log.Printf("ui: refresh orgs failed: %v", err)
		s.cacheMu.Lock()
		s.refresh = false
		s.cacheMu.Unlock()
		return
	}
	mappings, err := s.store.ListMappings()
	if err != nil {
		log.Printf("ui: refresh mappings failed: %v", err)
		s.cacheMu.Lock()
		s.refresh = false
		s.cacheMu.Unlock()
		return
	}

	cache := externalCache{
		refreshedAt: time.Now().UTC(),
	}
	cache.grafanaTeams, cache.grafanaTeamsErr = s.loadGrafanaTeams(orgs, mappings)
	cache.grafanaUsers, cache.grafanaUsersErr = s.loadGrafanaUsers(orgs)
	cache.entraGroups, cache.entraGroupsErr = s.loadEntraGroups(orgs, mappings)
	cache.entraUsers, cache.entraUsersErr = s.loadEntraUsers()

	s.cacheMu.Lock()
	s.cache = cache
	s.refresh = false
	s.cacheMu.Unlock()
}

func (s *Server) getExternalData(orgs []store.Org, mappings []store.Mapping) ([]grafanaTeamView, string, []grafanaUserView, string, []entraGroupView, string, []entraUserView, string) {
	s.cacheMu.RLock()
	cache := s.cache
	s.cacheMu.RUnlock()

	if cache.refreshedAt.IsZero() {
		s.refreshExternalData()
		s.cacheMu.RLock()
		cache = s.cache
		s.cacheMu.RUnlock()
	} else if time.Since(cache.refreshedAt) > 30*time.Second {
		go s.refreshExternalData()
	}

	return cache.grafanaTeams, cache.grafanaTeamsErr, cache.grafanaUsers, cache.grafanaUsersErr, cache.entraGroups, cache.entraGroupsErr, cache.entraUsers, cache.entraUsersErr
}

func (s *Server) handleGrafanaSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	start := time.Now()

	data, err := s.buildPageData()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data.CurrentPage = "grafana"
	data.ContentTemplate = "content-grafana"
	if err := s.tmpl.ExecuteTemplate(w, "layout.html", data); err != nil {
		log.Printf("render error: %v", err)
	}
	log.Printf("ui: grafana settings rendered in %s", time.Since(start).Round(time.Millisecond))
}

func (s *Server) handleEntraSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	start := time.Now()

	data, err := s.buildPageData()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data.CurrentPage = "entra"
	data.ContentTemplate = "content-entra"
	if err := s.tmpl.ExecuteTemplate(w, "layout.html", data); err != nil {
		log.Printf("render error: %v", err)
	}
	log.Printf("ui: entra settings rendered in %s", time.Since(start).Round(time.Millisecond))
}

func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	orgs, err := s.store.ListOrgs()
	if err != nil {
		http.Error(w, "failed to load orgs", http.StatusInternalServerError)
		return
	}

	type windowCounts struct {
		Users int `json:"users"`
		Teams int `json:"teams"`
	}
	type orgStatus struct {
		OrgID            int64        `json:"org_id"`
		GrafanaOrgID     int64        `json:"grafana_org_id"`
		Name             string       `json:"name"`
		GrafanaAccessOK  bool         `json:"grafana_access_ok"`
		EntraAccessOK    bool         `json:"entra_access_ok"`
		LastGrafanaSync  string       `json:"last_grafana_sync"`
		LastEntraSync    string       `json:"last_entra_sync"`
		GrafanaUserTotal int          `json:"grafana_users_total"`
		ChangesToday     windowCounts `json:"changes_today"`
		Changes3Days     windowCounts `json:"changes_last_3_days"`
		Changes7Days     windowCounts `json:"changes_last_7_days"`
	}
	type apiStatus struct {
		GeneratedAt    string      `json:"generated_at"`
		GrafanaOK      bool        `json:"grafana_ok"`
		EntraOK        bool        `json:"entra_ok"`
		GrafanaLastOK  string      `json:"grafana_last_ok"`
		EntraLastOK    string      `json:"entra_last_ok"`
		Orgs           []orgStatus `json:"orgs"`
	}

	now := time.Now().UTC()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	since3d := now.AddDate(0, 0, -3)
	since7d := now.AddDate(0, 0, -7)

	entraOK := false
	if s.entra != nil {
		if _, err := s.entra.ListGroups(); err == nil {
			entraOK = true
		}
	}

	var orgStatuses []orgStatus
	grafanaOK := s.grafana != nil
	for _, org := range orgs {
		status := orgStatus{
			OrgID:        org.ID,
			GrafanaOrgID: org.GrafanaOrgID,
			Name:         org.Name,
			EntraAccessOK: entraOK,
		}
		if s.grafana != nil {
			users, err := s.grafana.ListOrgUsers(org.GrafanaOrgID)
			if err != nil {
				status.GrafanaAccessOK = false
				grafanaOK = false
			} else {
				status.GrafanaAccessOK = true
				status.GrafanaUserTotal = len(users)
			}
		} else {
			status.GrafanaAccessOK = false
			grafanaOK = false
		}

		if last, err := s.store.LatestSyncActionTime(org.ID); err == nil {
			status.LastGrafanaSync = formatTime(last)
		}
		if s.entra != nil {
			status.LastEntraSync = formatTime(s.entra.LastOK())
		} else {
			status.LastEntraSync = "never"
		}

		if count, err := s.store.CountDistinctUserChangesSince(org.ID, startOfDay); err == nil {
			status.ChangesToday.Users = count
		}
		if count, err := s.store.CountDistinctTeamChangesSince(org.ID, startOfDay); err == nil {
			status.ChangesToday.Teams = count
		}
		if count, err := s.store.CountDistinctUserChangesSince(org.ID, since3d); err == nil {
			status.Changes3Days.Users = count
		}
		if count, err := s.store.CountDistinctTeamChangesSince(org.ID, since3d); err == nil {
			status.Changes3Days.Teams = count
		}
		if count, err := s.store.CountDistinctUserChangesSince(org.ID, since7d); err == nil {
			status.Changes7Days.Users = count
		}
		if count, err := s.store.CountDistinctTeamChangesSince(org.ID, since7d); err == nil {
			status.Changes7Days.Teams = count
		}

		orgStatuses = append(orgStatuses, status)
	}

	grafanaLastOK := "never"
	if s.grafana != nil {
		grafanaLastOK = formatTime(s.grafana.LastOK())
	}
	entraLastOK := "never"
	if s.entra != nil {
		entraLastOK = formatTime(s.entra.LastOK())
	}

	resp := apiStatus{
		GeneratedAt:   now.Format(time.RFC3339),
		GrafanaOK:     grafanaOK,
		EntraOK:       entraOK,
		GrafanaLastOK: grafanaLastOK,
		EntraLastOK:   entraLastOK,
		Orgs:          orgStatuses,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("api: status encode failed: %v", err)
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
	if externalGroupID == "" && externalGroupName != "" && s.entra != nil {
		groups, err := s.entra.ListGroups()
		if err == nil {
			for _, group := range groups {
				if strings.EqualFold(group.DisplayName, externalGroupName) {
					externalGroupID = group.ID
					break
				}
			}
		}
	}
	if externalGroupID == "" {
		http.Error(w, "missing Entra group id", http.StatusBadRequest)
		return
	}
	teamRole := strings.ToLower(strings.TrimSpace(r.FormValue("team_role")))
	if teamRole != "admin" {
		teamRole = "member"
	}
	roleOverride := r.FormValue("role_override")
	_, err := s.store.CreateMapping(store.Mapping{
		OrgID:             orgID,
		GrafanaTeamName:   teamName,
		ExternalGroupID:   externalGroupID,
		ExternalGroupName: externalGroupName,
		TeamRole:          teamRole,
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

func (s *Server) handlePurgeMappings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.entra == nil {
		http.Error(w, "entra client not configured", http.StatusInternalServerError)
		return
	}
	groups, err := s.entra.ListGroups()
	if err != nil {
		http.Error(w, "failed to load entra groups", http.StatusInternalServerError)
		return
	}
	allowed := make([]string, 0, len(groups))
	for _, group := range groups {
		if matchEntraGroupName(group.DisplayName) {
			allowed = append(allowed, group.ID)
		}
	}
	if len(allowed) == 0 {
		http.Error(w, "no matching entra groups found; purge aborted", http.StatusBadRequest)
		return
	}
	deleted, err := s.store.DeleteMappingsNotInGroupIDs(allowed)
	if err != nil {
		http.Error(w, "failed to purge mappings", http.StatusInternalServerError)
		return
	}
	log.Printf("ui: purged mappings not in entra filter, deleted=%d", deleted)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleAutoSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	enabled := strings.EqualFold(r.FormValue("auto_sync"), "true")
	if err := s.store.SetAutoSyncEnabled(enabled); err != nil {
		http.Error(w, "failed to update auto sync setting", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.refreshExternalData()
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

func (s *Server) handleApplySelected(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	ids := r.Form["action_id"]
	if len(ids) == 0 {
		http.Error(w, "no actions selected", http.StatusBadRequest)
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
	allowed := map[int64]struct{}{}
	for _, raw := range ids {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
			allowed[id] = struct{}{}
		}
	}
	var selected []store.PlanAction
	for _, action := range plan.Actions {
		if _, ok := allowed[action.ID]; ok {
			if !isSelectableAction(action.ActionType) {
				continue
			}
			selected = append(selected, action)
		}
	}
	if len(selected) == 0 {
		http.Error(w, "no valid actions selected", http.StatusBadRequest)
		return
	}
	if err := s.store.UpdatePlanStatus(plan.ID, "applying-selected"); err != nil {
		log.Printf("plan status update failed: %v", err)
	}
	err = s.syncer.ApplyPlan(selected)
	s.syncer.RecordRun(err)
	if err != nil {
		_ = s.store.UpdatePlanStatus(plan.ID, "failed")
		http.Error(w, "apply failed", http.StatusInternalServerError)
		return
	}
	_ = s.store.UpdatePlanStatus(plan.ID, "applied-selected")
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
	start := time.Now()
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
			log.Printf("ui: grafana teams fetch failed for org %d: %v", org.GrafanaOrgID, err)
			errs = append(errs, fmt.Sprintf("org %d: %v", org.GrafanaOrgID, err))
			continue
		}
		log.Printf("ui: grafana teams fetched org=%d count=%d", org.GrafanaOrgID, len(teams))
		for _, team := range teams {
			var mapped []store.Mapping
			if team.ID > 0 {
				mapped = byID[fmt.Sprintf("%d:%d", org.ID, team.ID)]
			}
			if len(mapped) == 0 {
				mapped = byName[fmt.Sprintf("%d:%s", org.ID, strings.ToLower(team.Name))]
			}
			var groupIDs []string
			for _, entry := range mapped {
				if entry.ExternalGroupID != "" {
					groupIDs = append(groupIDs, entry.ExternalGroupID)
				}
			}
			memberCount := 0
			if team.ID > 0 {
				members, err := s.grafana.ListTeamMembers(team.ID)
				if err != nil {
					log.Printf("ui: grafana team members fetch failed team=%d: %v", team.ID, err)
				} else {
					memberCount = len(members)
				}
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
				MemberCount:  memberCount,
				GroupIDsCSV:  strings.Join(groupIDs, ","),
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
	log.Printf("ui: grafana teams total=%d in %s", len(views), time.Since(start).Round(time.Millisecond))
	return views, strings.Join(errs, "; ")
}

func (s *Server) loadGrafanaUsers(orgs []store.Org) ([]grafanaUserView, string) {
	if s.grafana == nil {
		return nil, "grafana client not configured"
	}
	start := time.Now()
	teamLabelsByUser := map[int64]map[string]struct{}{}
	for _, org := range orgs {
		teams, err := s.grafana.ListTeams(org.GrafanaOrgID)
		if err != nil {
			log.Printf("ui: grafana teams fetch failed for org %d: %v", org.GrafanaOrgID, err)
			continue
		}
		for _, team := range teams {
			members, err := s.grafana.ListTeamMembers(team.ID)
			if err != nil {
				log.Printf("ui: grafana team members fetch failed team=%d: %v", team.ID, err)
				continue
			}
			for _, member := range members {
				if member.ID == 0 {
					continue
				}
				label := fmt.Sprintf("%s (%s)", team.Name, formatTeamRole(member.Role))
				if teamLabelsByUser[member.ID] == nil {
					teamLabelsByUser[member.ID] = map[string]struct{}{}
				}
				teamLabelsByUser[member.ID][label] = struct{}{}
			}
		}
	}
	users, err := s.grafana.ListAdminUsers()
	if err != nil {
		log.Printf("ui: grafana admin users fetch failed: %v", err)
		userByID := map[int64]grafanaUserView{}
		for _, org := range orgs {
			orgUsers, err := s.grafana.ListOrgUsers(org.GrafanaOrgID)
			if err != nil {
				log.Printf("ui: grafana org users fetch failed org=%d: %v", org.GrafanaOrgID, err)
				continue
			}
			for _, user := range orgUsers {
				if user.ID == 0 {
					continue
				}
				teams := joinTeamLabels(teamLabelsByUser[user.ID])
				userByID[user.ID] = grafanaUserView{
					ID:    user.ID,
					Login: user.Login,
					Email: user.Email,
					Name:  user.Name,
					Teams: teams,
				}
			}
		}
		views := make([]grafanaUserView, 0, len(userByID))
		for _, view := range userByID {
			views = append(views, view)
		}
		sort.Slice(views, func(i, j int) bool {
			return strings.ToLower(views[i].Login) < strings.ToLower(views[j].Login)
		})
		log.Printf("ui: grafana users total=%d in %s (org fallback)", len(views), time.Since(start).Round(time.Millisecond))
		return views, ""
	}
	views := make([]grafanaUserView, 0, len(users))
	for _, user := range users {
		teams := joinTeamLabels(teamLabelsByUser[user.ID])
		views = append(views, grafanaUserView{
			ID:    user.ID,
			Login: user.Login,
			Email: user.Email,
			Name:  user.Name,
			Teams: teams,
		})
	}
	sort.Slice(views, func(i, j int) bool {
		return strings.ToLower(views[i].Login) < strings.ToLower(views[j].Login)
	})
	log.Printf("ui: grafana users total=%d in %s", len(views), time.Since(start).Round(time.Millisecond))
	return views, ""
}

func (s *Server) loadEntraGroups(orgs []store.Org, mappings []store.Mapping) ([]entraGroupView, string) {
	if s.entra == nil {
		return nil, "entra client not configured"
	}
	start := time.Now()
	groups, err := s.entra.ListGroups()
	if err != nil {
		log.Printf("ui: entra groups fetch failed: %v", err)
		return nil, err.Error()
	}
	total := len(groups)
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
		if !matchEntraGroupName(group.DisplayName) {
			continue
		}
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
	log.Printf("ui: entra groups filtered=%d total=%d in %s", len(views), total, time.Since(start).Round(time.Millisecond))
	return views, ""
}

func matchEntraGroupName(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	return strings.HasPrefix(lower, "gapp_") && strings.Contains(lower, "_grf_")
}

func (s *Server) loadEntraUsers() ([]entraUserView, string) {
	if s.entra == nil {
		return nil, "entra client not configured"
	}
	start := time.Now()
	groups, err := s.entra.ListGroups()
	if err != nil {
		log.Printf("ui: entra users list groups failed: %v", err)
		return nil, err.Error()
	}
	type memberInfo struct {
		member   entra.Member
		groupsBy map[string]struct{}
	}
	seen := map[string]*memberInfo{}
	for _, group := range groups {
		if !matchEntraGroupName(group.DisplayName) {
			continue
		}
		members, err := s.entra.ListGroupMembers(group.ID)
		if err != nil {
			log.Printf("ui: entra members fetch failed group=%s: %v", group.ID, err)
			continue
		}
		for _, member := range members {
			if member.ID == "" {
				continue
			}
			if seen[member.ID] == nil {
				seen[member.ID] = &memberInfo{
					member:   member,
					groupsBy: map[string]struct{}{},
				}
			}
			seen[member.ID].groupsBy[group.DisplayName] = struct{}{}
		}
	}
	views := make([]entraUserView, 0, len(seen))
	for _, info := range seen {
		member := info.member
		views = append(views, entraUserView{
			ID:          member.ID,
			DisplayName: member.DisplayName,
			Mail:        member.Mail,
			UPN:         member.UPN,
			Department:  member.Department,
			Groups:      joinGroupLabels(info.groupsBy),
		})
	}
	sort.Slice(views, func(i, j int) bool {
		return strings.ToLower(views[i].DisplayName) < strings.ToLower(views[j].DisplayName)
	})
	log.Printf("ui: entra users filtered=%d in %s", len(views), time.Since(start).Round(time.Millisecond))
	return views, ""
}

func (s *Server) handleEntraGroupMembers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.entra == nil {
		http.Error(w, "entra client not configured", http.StatusInternalServerError)
		return
	}
	groupID := strings.TrimSpace(r.URL.Query().Get("group_id"))
	if groupID == "" {
		http.Error(w, "missing group_id", http.StatusBadRequest)
		return
	}
	members, err := s.entra.ListGroupMembers(groupID)
	if err != nil {
		http.Error(w, "failed to list group members", http.StatusInternalServerError)
		return
	}
	type memberView struct {
		DisplayName string `json:"displayName"`
		UPN         string `json:"upn"`
		Department  string `json:"department"`
		Mail        string `json:"mail"`
	}
	result := make([]memberView, 0, len(members))
	for _, member := range members {
		result = append(result, memberView{
			DisplayName: member.DisplayName,
			UPN:         member.UPN,
			Department:  member.Department,
			Mail:        member.Mail,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		return strings.ToLower(result[i].DisplayName) < strings.ToLower(result[j].DisplayName)
	})
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		log.Printf("ui: members encode failed: %v", err)
	}
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

func buildPlanGroups(actions []store.PlanAction) []planTeamGroup {
	type group struct {
		title   string
		actions []planActionView
	}
	groups := map[string]*group{}
	order := []string{}
	for _, action := range actions {
		title := action.TeamName
		if title == "" {
			if action.GrafanaOrgID != 0 {
				title = fmt.Sprintf("Org %d", action.GrafanaOrgID)
			} else {
				title = "Org actions"
			}
		}
		if groups[title] == nil {
			groups[title] = &group{title: title}
			order = append(order, title)
		}
		groups[title].actions = append(groups[title].actions, planActionView{
			ID:         action.ID,
			Type:       action.ActionType,
			OrgID:      action.GrafanaOrgID,
			Team:       action.TeamName,
			Email:      action.Email,
			Role:       action.Role,
			TeamRole:   action.TeamRole,
			Note:       action.Note,
			Class:      actionClass(action.ActionType),
			Selectable: isSelectableAction(action.ActionType),
		})
	}
	var result []planTeamGroup
	for _, title := range order {
		g := groups[title]
		result = append(result, planTeamGroup{Title: g.title, Actions: g.actions})
	}
	return result
}

func formatTeamRole(role string) string {
	if strings.EqualFold(role, "admin") {
		return "Admin"
	}
	return "Member"
}

func joinTeamLabels(labels map[string]struct{}) string {
	if len(labels) == 0 {
		return ""
	}
	values := make([]string, 0, len(labels))
	for label := range labels {
		values = append(values, label)
	}
	sort.Strings(values)
	return strings.Join(values, ", ")
}

func joinGroupLabels(labels map[string]struct{}) string {
	if len(labels) == 0 {
		return ""
	}
	values := make([]string, 0, len(labels))
	for label := range labels {
		values = append(values, label)
	}
	sort.Strings(values)
	return strings.Join(values, ", ")
}

func actionClass(actionType string) string {
	switch actionType {
	case "remove_user_from_team":
		return "danger"
	case "blocked_create_user":
		return "muted"
	default:
		return "success"
	}
}

func actionLabel(actionType string) string {
	switch actionType {
	case "create_team":
		return "Create team"
	case "create_user":
		return "Create user"
	case "add_user_to_org":
		return "Add to org"
	case "update_user_role":
		return "Update org role"
	case "add_user_to_team":
		return "Add to team"
	case "update_team_role":
		return "Update team role"
	case "remove_user_from_team":
		return "Remove from team"
	case "blocked_create_user":
		return "Blocked create user"
	default:
		return actionType
	}
}

func isSelectableAction(actionType string) bool {
	switch actionType {
	case "blocked_create_user":
		return false
	default:
		return true
	}
}
