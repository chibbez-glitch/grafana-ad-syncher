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
	folderPerms     []folderPermGroup
	folderPermsErr  string
	orphans         []orphanView
	orphansHidden   int
	orphansErr      string
	refreshedAt     time.Time
}

// orphanView is a Grafana account the sync cannot account for: the person is
// gone from Entra, disabled there, or in none of the mapped groups.
//
// This service never deletes or disables Grafana accounts - its only
// destructive action is removing someone from a team - so this is a worklist
// for a human, not something that gets applied. Treat it as a starting point
// and not a verdict: guest accounts from another tenant and service accounts
// legitimately appear here.
type orphanView struct {
	Login  string
	Email  string
	Name   string
	Teams  string
	Status string
	Detail string
	Class  string
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
	FolderPerms      []folderPermGroup
	Orphans          []orphanView
	OrphansErr       string
	OrphansHidden    int
	FolderPermsErr   string
	PlanGroups       []planTeamGroup
	SyncIssues       []syncIssueView
	SyncErrorCount   int
	SyncWarnCount    int
	LastRun          string
	LastStatus       string
	Plan             *store.Plan
	AutoSyncEnabled  bool
	PlanCreatedAt    string
	AutoSyncPossible bool
	SyncIntervalText string
	CurrentPage      string
	ContentTemplate  string
}

// syncIssueView is one problem from the last plan or apply, rendered in the
// "Sync Issues" panel so failures stop being log-only.
type syncIssueView struct {
	At       string
	Phase    string
	Severity string
	Scope    string
	Email    string
	Message  string
	Class    string
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

type folderPermEntry struct {
	Subject     string
	SubjectType string
	Permission  string
}

type folderPermGroup struct {
	OrgID       int64
	OrgName     string
	FolderUID   string
	FolderTitle string
	Entries     []folderPermEntry
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
		filepath.Join(templateDir, "folders.html"),
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
	mux.HandleFunc("/folders", s.handleFolderPermissions)
	mux.HandleFunc("/api/status", s.handleAPIStatus)
	mux.HandleFunc("/sync/fetch", s.handleFetch)
	mux.HandleFunc("/orgs", s.handleCreateOrg)
	mux.HandleFunc("/orgs/delete", s.handleDeleteOrg)
	mux.HandleFunc("/mappings", s.handleCreateMapping)
	mux.HandleFunc("/mappings/delete", s.handleDeleteMapping)
	mux.HandleFunc("/mappings/update", s.handleUpdateMapping)
	mux.HandleFunc("/mappings/purge", s.handlePurgeMappings)
	mux.HandleFunc("/entra/group/members", s.handleEntraGroupMembers)
	mux.HandleFunc("/settings/auto-sync", s.handleAutoSync)
	mux.HandleFunc("/api/sync/pending", s.handleSyncPending)
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
		return pageData{}, fmt.Errorf("failed to load orgs: %w", err)
	}
	mappings, err := s.store.ListMappings()
	if err != nil {
		return pageData{}, fmt.Errorf("failed to load mappings: %w", err)
	}
	plan, err := s.store.LatestPlan()
	if err != nil {
		return pageData{}, fmt.Errorf("failed to load plan: %w", err)
	}
	grafanaTeams, grafanaTeamsErr, grafanaUsers, grafanaUsersErr, entraGroups, entraGroupsErr, entraUsers, entraUsersErr, folderPerms, folderPermsErr := s.getExternalData(orgs, mappings)
	var planGroups []planTeamGroup
	var planCreatedAt string
	if plan != nil {
		planGroups = buildPlanGroups(plan.Actions)
		// The store keeps CreatedAt as an RFC3339 string, so it needs converting
		// separately from the time.Time values above.
		planCreatedAt = formatStoredTime(plan.CreatedAt)
	}
	orphans, orphansHidden, orphansErr := s.cachedOrphans()
	syncIssues, syncErrors, syncWarnings := buildSyncIssues(s.syncer.Issues())
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
		FolderPerms:     folderPerms,
		FolderPermsErr:  folderPermsErr,
		Orphans:         orphans,
		OrphansErr:      orphansErr,
		OrphansHidden:   orphansHidden,
		PlanGroups:      planGroups,
		SyncIssues:      syncIssues,
		SyncErrorCount:  syncErrors,
		SyncWarnCount:   syncWarnings,
		LastRun:         formatTime(lastRun),
		LastStatus:      lastStatus,
		Plan:            plan,
		AutoSyncEnabled: autoSyncEnabled,
		PlanCreatedAt:    planCreatedAt,
		AutoSyncPossible: autoSyncInterval > 0,
		SyncIntervalText: autoSyncInterval.String(),
	}, nil
}

// buildSyncIssues renders the syncer's issues for the template and counts them
// by severity so the panel header can lead with the number that matters.
// writeApplyError renders the apply failure with the individual issues inline.
//
// The bare "apply failed: 3 of 79 actions failed, see sync issues" that used to
// be returned here meant leaving the page, finding the panel, and hoping the
// next refresh had not already overwritten it. The failures are what the
// operator needs, so put them on the page that reports the failure.
func (s *Server) writeApplyError(w http.ResponseWriter, err error) {
	issues := s.syncer.Issues()
	applied := make([]syncer.Issue, 0, len(issues))
	for _, issue := range issues {
		if issue.Phase == syncer.PhaseApply {
			applied = append(applied, issue)
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusInternalServerError)

	var sb strings.Builder
	sb.WriteString(`<!doctype html><meta charset="utf-8"><title>Apply failed</title>`)
	sb.WriteString(`<link rel="stylesheet" href="/static/style.css">`)
	sb.WriteString(`<div style="max-width:1100px;margin:2rem auto;padding:0 1rem;font-family:system-ui,sans-serif">`)
	sb.WriteString(`<h1>Apply failed</h1><p>`)
	sb.WriteString(template.HTMLEscapeString(err.Error()))
	sb.WriteString(`</p>`)

	if len(applied) == 0 {
		sb.WriteString(`<p>No per-action detail was recorded.</p>`)
	} else {
		sb.WriteString(`<table style="width:100%;border-collapse:collapse">`)
		sb.WriteString(`<tr><th align="left">Severity</th><th align="left">Scope</th>` +
			`<th align="left">User</th><th align="left">Detail</th></tr>`)
		for _, issue := range applied {
			colour := "#b00020"
			if issue.Severity != syncer.SeverityError {
				colour = "#8a6d00"
			}
			sb.WriteString(`<tr style="border-top:1px solid #ddd;vertical-align:top">`)
			sb.WriteString(`<td style="color:` + colour + `;padding:.4rem .6rem .4rem 0">` +
				template.HTMLEscapeString(issue.Severity) + `</td>`)
			sb.WriteString(`<td style="padding:.4rem .6rem .4rem 0">` + template.HTMLEscapeString(issue.Scope) + `</td>`)
			sb.WriteString(`<td style="padding:.4rem .6rem .4rem 0">` + template.HTMLEscapeString(issue.Email) + `</td>`)
			sb.WriteString(`<td style="padding:.4rem 0">` + template.HTMLEscapeString(issue.Message) + `</td>`)
			sb.WriteString(`</tr>`)
		}
		sb.WriteString(`</table>`)
	}

	sb.WriteString(`<p style="margin-top:1.5rem"><a href="/">Back to the plan</a>` +
		` &middot; re-run <strong>Preview sync</strong> to see what is still outstanding.</p></div>`)
	_, _ = w.Write([]byte(sb.String()))
}

func buildSyncIssues(issues []syncer.Issue) ([]syncIssueView, int, int) {
	views := make([]syncIssueView, 0, len(issues))
	errorCount := 0
	warnCount := 0
	for _, issue := range issues {
		class := "danger"
		if issue.Severity == syncer.SeverityError {
			errorCount++
		} else {
			warnCount++
			class = "muted"
		}
		views = append(views, syncIssueView{
			At:       formatTime(issue.At),
			Phase:    issue.Phase,
			Severity: issue.Severity,
			Scope:    issue.Scope,
			Email:    issue.Email,
			Message:  issue.Message,
			Class:    class,
		})
	}
	return views, errorCount, warnCount
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
	cache.folderPerms, cache.folderPermsErr = s.loadGrafanaFolderPermissions(orgs)
	cache.orphans, cache.orphansHidden, cache.orphansErr = s.loadOrphans(cache.grafanaUsers, cache.entraUsers)

	s.cacheMu.Lock()
	s.cache = cache
	s.refresh = false
	s.cacheMu.Unlock()
}

// reviewExcluded holds the logins and e-mails kept out of the review panel,
// lower-cased. Set once at startup.
var reviewExcluded = map[string]struct{}{}

// SetReviewExclusions configures which accounts the review panel ignores.
//
// Service and break-glass accounts have no Entra person behind them by design.
// Without this they sit in the panel permanently, and a list with permanent
// entries stops being read - the same way the four org-role warnings did.
func SetReviewExclusions(names []string) {
	next := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "" {
			next[name] = struct{}{}
		}
	}
	reviewExcluded = next
}

func isReviewExcluded(login, email string) bool {
	if _, ok := reviewExcluded[strings.ToLower(strings.TrimSpace(login))]; ok {
		return true
	}
	_, ok := reviewExcluded[strings.ToLower(strings.TrimSpace(email))]
	return ok
}

// cachedOrphans reads the orphan list from the cache. Callers reach it after
// getExternalData, which is what keeps the cache fresh.
func (s *Server) cachedOrphans() ([]orphanView, int, string) {
	s.cacheMu.RLock()
	defer s.cacheMu.RUnlock()
	return s.cache.orphans, s.cache.orphansHidden, s.cache.orphansErr
}

// loadOrphans lists Grafana accounts that no longer correspond to an active
// Entra person in a mapped group.
//
// It needs the tenant-wide user list, not just the members of the mapped
// groups, because those two answer different questions: "in no mapped group"
// is routine, "not in Entra at all" is a leaver. Telling them apart is the
// whole point of the panel, and the mapped-group membership alone cannot.
func (s *Server) loadOrphans(grafanaUsers []grafanaUserView, entraUsers []entraUserView) ([]orphanView, int, string) {
	if s.entra == nil {
		return nil, 0, "entra client not configured"
	}
	if len(grafanaUsers) == 0 {
		return nil, 0, ""
	}
	start := time.Now()

	tenant, err := s.entra.ListUsers()
	if err != nil {
		log.Printf("ui: orphan check: entra user list failed: %v", err)
		return nil, 0, err.Error()
	}

	// Both mail and UPN, because Grafana stores whichever one the login provider
	// sent, and in this tenant that differs per account.
	type tenantUser struct {
		enabled bool
		display string
	}
	byAddress := make(map[string]tenantUser, len(tenant)*2)
	for _, user := range tenant {
		entry := tenantUser{enabled: user.AccountEnabled, display: user.DisplayName}
		for _, address := range []string{user.Mail, user.UPN} {
			address = strings.TrimSpace(strings.ToLower(address))
			if address != "" {
				byAddress[address] = entry
			}
		}
	}

	inMappedGroup := make(map[string]struct{}, len(entraUsers)*2)
	for _, user := range entraUsers {
		for _, address := range []string{user.Mail, user.UPN} {
			address = strings.TrimSpace(strings.ToLower(address))
			if address != "" {
				inMappedGroup[address] = struct{}{}
			}
		}
	}

	out := make([]orphanView, 0)
	hidden := 0
	for _, user := range grafanaUsers {
		email := strings.TrimSpace(strings.ToLower(user.Email))
		login := strings.TrimSpace(strings.ToLower(user.Login))

		if _, ok := inMappedGroup[email]; ok {
			continue
		}
		if _, ok := inMappedGroup[login]; ok {
			continue
		}
		// Counted, not silently dropped: the panel says how many it is hiding, so
		// nobody has to wonder why an account never shows up.
		if isReviewExcluded(login, email) {
			hidden++
			continue
		}

		entry, found := byAddress[email]
		if !found {
			entry, found = byAddress[login]
		}

		view := orphanView{Login: user.Login, Email: user.Email, Name: user.Name, Teams: user.Teams}
		switch {
		case !found:
			view.Status = "not in Entra"
			view.Class = "danger"
			view.Detail = "No Entra account matches this mail or UPN. Usually a leaver - but guests from another tenant can also sit under a different UPN, so check before removing."
		case !entry.enabled:
			view.Status = "disabled in Entra"
			view.Class = "danger"
			view.Detail = "The Entra account exists but is disabled. The person cannot sign in; the Grafana account and its team memberships remain."
		default:
			view.Status = "no mapped group"
			view.Class = "muted"
			view.Detail = "Active in Entra, but in none of the mapped _grf_ groups. Expected for service and admin accounts - not by itself a reason to remove anything."
		}
		out = append(out, view)
	}

	sort.Slice(out, func(i, j int) bool {
		// Worst first: a leaver matters more than an account that simply is not
		// in a mapped group.
		if out[i].Status != out[j].Status {
			return orphanRank(out[i].Status) < orphanRank(out[j].Status)
		}
		return out[i].Login < out[j].Login
	})

	log.Printf("ui: orphan check: %d of %d grafana accounts unaccounted for, %d entra users scanned in %s",
		len(out), len(grafanaUsers), len(tenant), time.Since(start).Round(time.Millisecond))
	return out, hidden, ""
}

func orphanRank(status string) int {
	switch status {
	case "not in Entra":
		return 0
	case "disabled in Entra":
		return 1
	default:
		return 2
	}
}

func (s *Server) getExternalData(orgs []store.Org, mappings []store.Mapping) ([]grafanaTeamView, string, []grafanaUserView, string, []entraGroupView, string, []entraUserView, string, []folderPermGroup, string) {
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

	return cache.grafanaTeams, cache.grafanaTeamsErr, cache.grafanaUsers, cache.grafanaUsersErr, cache.entraGroups, cache.entraGroupsErr, cache.entraUsers, cache.entraUsersErr, cache.folderPerms, cache.folderPermsErr
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

func (s *Server) handleFolderPermissions(w http.ResponseWriter, r *http.Request) {
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
	data.CurrentPage = "folders"
	data.ContentTemplate = "content-folders"
	if err := s.tmpl.ExecuteTemplate(w, "layout.html", data); err != nil {
		log.Printf("render error: %v", err)
	}
	log.Printf("ui: folder permissions rendered in %s", time.Since(start).Round(time.Millisecond))
}

func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	orgs, err := s.store.ListOrgs()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to load orgs: %v", err), http.StatusInternalServerError)
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

func (s *Server) handleSyncPending(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	type pendingStatus struct {
		GeneratedAt    string `json:"generated_at"`
		PlanID         int64  `json:"plan_id"`
		PlanStatus     string `json:"plan_status"`
		TotalActions   int    `json:"total_actions"`
		PendingActions int    `json:"pending_actions"`
	}

	plan, err := s.store.LatestPlan()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to load plan: %v", err), http.StatusInternalServerError)
		return
	}

	var planID int64
	var status string
	totalActions := 0
	pendingActions := 0
	if plan != nil {
		planID = plan.ID
		status = plan.Status
		totalActions = len(plan.Actions)
		pendingActions = totalActions
		if status == "applied" || status == "applied-selected" {
			pendingActions = 0
		}
	}

	resp := pendingStatus{
		GeneratedAt:    time.Now().UTC().Format(time.RFC3339),
		PlanID:         planID,
		PlanStatus:     status,
		TotalActions:   totalActions,
		PendingActions: pendingActions,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("api: sync pending encode failed: %v", err)
	}
}

func (s *Server) handleCreateOrg(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, fmt.Sprintf("invalid form: %v", err), http.StatusBadRequest)
		return
	}
	orgID, err := strconv.ParseInt(r.FormValue("grafana_org_id"), 10, 64)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid grafana_org_id: %v", err), http.StatusBadRequest)
		return
	}
	name := r.FormValue("name")
	defaultRole := r.FormValue("default_role")
	if defaultRole == "" {
		defaultRole = "Viewer"
	}
	_, err = s.store.CreateOrg(store.Org{GrafanaOrgID: orgID, Name: name, DefaultRole: defaultRole})
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to create org: %v", err), http.StatusBadRequest)
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
		http.Error(w, fmt.Sprintf("invalid form: %v", err), http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid org id: %v", err), http.StatusBadRequest)
		return
	}
	if err := s.store.DeleteOrg(id); err != nil {
		http.Error(w, fmt.Sprintf("failed to delete org: %v", err), http.StatusBadRequest)
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
		http.Error(w, fmt.Sprintf("invalid form: %v", err), http.StatusBadRequest)
		return
	}
	orgID, err := strconv.ParseInt(r.FormValue("org_id"), 10, 64)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid org id: %v", err), http.StatusBadRequest)
		return
	}
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
	_, err = s.store.CreateMapping(store.Mapping{
		OrgID:             orgID,
		GrafanaTeamName:   teamName,
		ExternalGroupID:   externalGroupID,
		ExternalGroupName: externalGroupName,
		TeamRole:          teamRole,
		RoleOverride:      roleOverride,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to create mapping: %v", err), http.StatusBadRequest)
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
		http.Error(w, fmt.Sprintf("invalid form: %v", err), http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid mapping id: %v", err), http.StatusBadRequest)
		return
	}
	if err := s.store.DeleteMapping(id); err != nil {
		http.Error(w, fmt.Sprintf("failed to delete mapping: %v", err), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleUpdateMapping(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, fmt.Sprintf("invalid form: %v", err), http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid mapping id: %v", err), http.StatusBadRequest)
		return
	}
	orgID, err := strconv.ParseInt(r.FormValue("org_id"), 10, 64)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid org id: %v", err), http.StatusBadRequest)
		return
	}
	teamName := r.FormValue("grafana_team_name")
	externalGroupID := r.FormValue("external_group_id")
	externalGroupName := r.FormValue("external_group_name")
	var existingMapping *store.Mapping
	if id != 0 {
		if existing, err := s.store.GetMapping(id); err == nil && existing != nil {
			existingMapping = existing
		}
	}
	if externalGroupID == "" && externalGroupName == "" && id != 0 {
		if existingMapping != nil {
			externalGroupID = existingMapping.ExternalGroupID
			externalGroupName = existingMapping.ExternalGroupName
		}
	}
	if externalGroupID == "" && externalGroupName != "" {
		orgs, orgErr := s.store.ListOrgs()
		mappings, mapErr := s.store.ListMappings()
		if orgErr == nil && mapErr == nil {
			_, _, _, _, entraGroups, _, _, _, _, _ := s.getExternalData(orgs, mappings)
			for _, group := range entraGroups {
				if strings.EqualFold(group.DisplayName, externalGroupName) {
					externalGroupID = group.ID
					break
				}
			}
		}
	}
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
	teamID := int64(0)
	if existingMapping != nil && existingMapping.OrgID == orgID && strings.EqualFold(existingMapping.GrafanaTeamName, teamName) {
		teamID = existingMapping.GrafanaTeamID
	}
	if err := s.store.UpdateMapping(store.Mapping{
		ID:                id,
		OrgID:             orgID,
		GrafanaTeamName:   teamName,
		GrafanaTeamID:     teamID,
		ExternalGroupID:   externalGroupID,
		ExternalGroupName: externalGroupName,
		TeamRole:          teamRole,
		RoleOverride:      roleOverride,
	}); err != nil {
		http.Error(w, fmt.Sprintf("failed to update mapping: %v", err), http.StatusBadRequest)
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
		http.Error(w, fmt.Sprintf("failed to load entra groups: %v", err), http.StatusInternalServerError)
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
		http.Error(w, fmt.Sprintf("failed to purge mappings: %v", err), http.StatusInternalServerError)
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
		http.Error(w, fmt.Sprintf("invalid form: %v", err), http.StatusBadRequest)
		return
	}
	enabled := strings.EqualFold(r.FormValue("auto_sync"), "true")
	if err := s.store.SetAutoSyncEnabled(enabled); err != nil {
		http.Error(w, fmt.Sprintf("failed to update auto sync setting: %v", err), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.resolveTeamIDs()
	s.refreshExternalData()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) resolveTeamIDs() {
	if s.grafana == nil {
		return
	}
	orgs, err := s.store.ListOrgs()
	if err != nil {
		log.Printf("ui: resolve team ids failed to load orgs: %v", err)
		return
	}
	mappings, err := s.store.ListMappings()
	if err != nil {
		log.Printf("ui: resolve team ids failed to load mappings: %v", err)
		return
	}
	orgByID := map[int64]store.Org{}
	for _, org := range orgs {
		orgByID[org.ID] = org
	}
	for _, mapping := range mappings {
		org, ok := orgByID[mapping.OrgID]
		if !ok || strings.TrimSpace(mapping.GrafanaTeamName) == "" {
			continue
		}
		teamID, found, err := s.grafana.SearchTeam(org.GrafanaOrgID, mapping.GrafanaTeamName)
		if err != nil {
			log.Printf("ui: resolve team id failed org=%d team=%s: %v", org.GrafanaOrgID, mapping.GrafanaTeamName, err)
			continue
		}
		if !found || teamID == 0 {
			continue
		}
		if mapping.GrafanaTeamID != teamID {
			if err := s.store.UpdateMappingTeamID(mapping.ID, teamID); err != nil {
				log.Printf("ui: resolve team id update failed mapping=%d team=%d: %v", mapping.ID, teamID, err)
			}
		}
	}
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	plan, err := s.syncer.BuildPlan()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to build plan: %v", err), http.StatusInternalServerError)
		return
	}
	if _, err := s.store.ReplacePlan(*plan); err != nil {
		http.Error(w, fmt.Sprintf("failed to store plan: %v", err), http.StatusInternalServerError)
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
		http.Error(w, fmt.Sprintf("failed to build plan: %v", err), http.StatusInternalServerError)
		return
	}
	planID, err := s.store.ReplacePlan(*plan)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to store plan: %v", err), http.StatusInternalServerError)
		return
	}
	if err := s.store.UpdatePlanStatus(planID, "applying"); err != nil {
		log.Printf("plan status update failed: %v", err)
	}
	err = s.syncer.ApplyPlan(plan.Actions)
	s.syncer.RecordRun(err)
	if err != nil {
		_ = s.store.UpdatePlanStatus(planID, "failed")
		s.writeApplyError(w, err)
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
		http.Error(w, fmt.Sprintf("failed to load plan: %v", err), http.StatusInternalServerError)
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
		s.writeApplyError(w, err)
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
		http.Error(w, fmt.Sprintf("invalid form: %v", err), http.StatusBadRequest)
		return
	}
	ids := r.Form["action_id"]
	if len(ids) == 0 {
		http.Error(w, "no actions selected", http.StatusBadRequest)
		return
	}
	plan, err := s.store.LatestPlan()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to load plan: %v", err), http.StatusInternalServerError)
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
		s.writeApplyError(w, err)
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
		http.Error(w, fmt.Sprintf("failed to clear plan: %v", err), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// displayLoc is the timezone the UI renders timestamps in. Storage, logging and
// the Grafana/Graph APIs all stay UTC - this only changes what a human reads.
var displayLoc = time.UTC

// autoSyncInterval mirrors SYNC_INTERVAL so the UI can tell the operator when
// the "Automatische Updates" toggle cannot do anything. At 0 the scheduler
// goroutine is never started, and the toggle then sets a flag nothing reads.
var autoSyncInterval time.Duration

// SetAutoSyncInterval tells the UI what SYNC_INTERVAL the process was started
// with.
func SetAutoSyncInterval(d time.Duration) { autoSyncInterval = d }

// SetDisplayLocation switches the UI timezone, by IANA name (Europe/Luxembourg).
// An empty name or "UTC" keeps UTC.
//
// The tz database is embedded in the binary via the time/tzdata import in main,
// so this resolves on a bare alpine image with no tzdata package installed.
func SetDisplayLocation(name string) error {
	name = strings.TrimSpace(name)
	if name == "" || strings.EqualFold(name, "UTC") {
		displayLoc = time.UTC
		return nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return err
	}
	displayLoc = loc
	return nil
}

// DisplayLocation is the timezone currently used for rendering, for startup
// logging.
func DisplayLocation() string { return displayLoc.String() }

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	// The zone abbreviation is deliberate: "13:04:27 CEST" cannot be misread as
	// UTC, which a bare local time silently can.
	return t.In(displayLoc).Format("2006-01-02 15:04:05 MST")
}

// formatStoredTime renders an RFC3339 string from the store in the display
// timezone. Anything that does not parse is passed through unchanged rather
// than replaced by an error, so a stray value stays visible.
func formatStoredTime(value string) string {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return value
	}
	return formatTime(t)
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
	users, err := s.grafana.ListAllUsers()
	if err != nil {
		log.Printf("ui: grafana user list fetch failed, falling back to org member lists: %v", err)
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

func (s *Server) loadGrafanaFolderPermissions(orgs []store.Org) ([]folderPermGroup, string) {
	if s.grafana == nil {
		return nil, "grafana client not configured"
	}
	start := time.Now()
	var groups []folderPermGroup
	var errs []string
	for _, org := range orgs {
		folders, err := s.grafana.ListFolders(org.GrafanaOrgID)
		if err != nil {
			log.Printf("ui: grafana folders fetch failed for org %d: %v", org.GrafanaOrgID, err)
			errs = append(errs, fmt.Sprintf("org %d: %v", org.GrafanaOrgID, err))
			continue
		}
		for _, folder := range folders {
			perms, err := s.grafana.ListFolderPermissions(org.GrafanaOrgID, folder.UID)
			if err != nil {
				log.Printf("ui: grafana folder permissions fetch failed org=%d folder=%s: %v", org.GrafanaOrgID, folder.UID, err)
				errs = append(errs, fmt.Sprintf("org %d folder %s: %v", org.GrafanaOrgID, folder.UID, err))
				continue
			}
			group := folderPermGroup{
				OrgID:       org.GrafanaOrgID,
				OrgName:     org.Name,
				FolderUID:   folder.UID,
				FolderTitle: folder.Title,
			}
			for _, perm := range perms {
				subject, subjectType := folderPermissionSubject(perm)
				entry := folderPermEntry{
					Subject:     subject,
					SubjectType: subjectType,
					Permission: perm.PermissionName,
				}
				group.Entries = append(group.Entries, entry)
			}
			sort.Slice(group.Entries, func(i, j int) bool {
				return strings.ToLower(group.Entries[i].Subject) < strings.ToLower(group.Entries[j].Subject)
			})
			groups = append(groups, group)
		}
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].OrgID == groups[j].OrgID {
			return strings.ToLower(groups[i].FolderTitle) < strings.ToLower(groups[j].FolderTitle)
		}
		return groups[i].OrgID < groups[j].OrgID
	})
	log.Printf("ui: grafana folder permissions total=%d in %s", len(groups), time.Since(start).Round(time.Millisecond))
	return groups, strings.Join(errs, "; ")
}

func folderPermissionSubject(perm grafana.FolderPermission) (string, string) {
	if perm.TeamID != 0 {
		name := perm.Team
		if strings.TrimSpace(name) == "" {
			name = fmt.Sprintf("team %d", perm.TeamID)
		}
		return name, "team"
	}
	if perm.UserID != 0 {
		name := perm.User
		if strings.TrimSpace(name) == "" {
			name = fmt.Sprintf("user %d", perm.UserID)
		}
		return name, "user"
	}
	if strings.TrimSpace(perm.Role) != "" {
		return perm.Role, "role"
	}
	return "unknown", "unknown"
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
		http.Error(w, fmt.Sprintf("failed to list group members: %v", err), http.StatusInternalServerError)
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
