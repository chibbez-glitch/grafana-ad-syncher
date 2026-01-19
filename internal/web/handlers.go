package web

import (
	"html/template"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"grafana-ad-syncher/internal/store"
	syncer "grafana-ad-syncher/internal/sync"
)

type Server struct {
	store  *store.Store
	syncer *syncer.Syncer
	tmpl   *template.Template
}

type pageData struct {
	Orgs       []store.Org
	Mappings   []store.Mapping
	LastRun    string
	LastStatus string
	Plan       *store.Plan
}

func New(store *store.Store, syncer *syncer.Syncer, templateDir string) (*Server, error) {
	tmpl, err := template.ParseFiles(
		filepath.Join(templateDir, "layout.html"),
		filepath.Join(templateDir, "index.html"),
	)
	if err != nil {
		return nil, err
	}
	return &Server{store: store, syncer: syncer, tmpl: tmpl}, nil
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/orgs", s.handleCreateOrg)
	mux.HandleFunc("/orgs/delete", s.handleDeleteOrg)
	mux.HandleFunc("/mappings", s.handleCreateMapping)
	mux.HandleFunc("/mappings/delete", s.handleDeleteMapping)
	mux.HandleFunc("/sync/preview", s.handlePreview)
	mux.HandleFunc("/sync/apply", s.handleApply)
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
	lastRun, lastStatus := s.syncer.LastRun()
	data := pageData{
		Orgs:       orgs,
		Mappings:   mappings,
		LastRun:    formatTime(lastRun),
		LastStatus: lastStatus,
		Plan:       plan,
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

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Format(time.RFC3339)
}
