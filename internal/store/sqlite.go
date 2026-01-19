package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Store struct {
	db *sql.DB
}

type Org struct {
	ID           int64
	GrafanaOrgID int64
	Name         string
	DefaultRole  string
}

type Mapping struct {
	ID                int64
	OrgID             int64
	GrafanaTeamName   string
	GrafanaTeamID     int64
	ExternalGroupID   string
	ExternalGroupName string
	RoleOverride      string
}

type Plan struct {
	ID        int64
	CreatedAt string
	Status    string
	Actions   []PlanAction
}

type PlanAction struct {
	ID             int64
	PlanID         int64
	ActionType     string
	OrgID          int64
	GrafanaOrgID   int64
	TeamID         int64
	TeamName       string
	UserID         int64
	Email          string
	DisplayName    string
	Role           string
	ExternalGroupID string
	Note           string
}

func Open(dataDir string) (*Store, error) {
	path := filepath.Join(dataDir, "sync.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) ListOrgs() ([]Org, error) {
	rows, err := s.db.Query(`SELECT id, grafana_org_id, name, default_role FROM orgs ORDER BY grafana_org_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var orgs []Org
	for rows.Next() {
		var org Org
		if err := rows.Scan(&org.ID, &org.GrafanaOrgID, &org.Name, &org.DefaultRole); err != nil {
			return nil, err
		}
		orgs = append(orgs, org)
	}
	return orgs, rows.Err()
}

func (s *Store) CreateOrg(org Org) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO orgs (grafana_org_id, name, default_role) VALUES (?, ?, ?)`, org.GrafanaOrgID, org.Name, org.DefaultRole)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) DeleteOrg(id int64) error {
	_, err := s.db.Exec(`DELETE FROM orgs WHERE id = ?`, id)
	return err
}

func (s *Store) ListMappings() ([]Mapping, error) {
	rows, err := s.db.Query(`SELECT id, org_id, grafana_team_name, grafana_team_id, external_group_id, external_group_name, role_override FROM mappings ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var mappings []Mapping
	for rows.Next() {
		var m Mapping
		if err := rows.Scan(&m.ID, &m.OrgID, &m.GrafanaTeamName, &m.GrafanaTeamID, &m.ExternalGroupID, &m.ExternalGroupName, &m.RoleOverride); err != nil {
			return nil, err
		}
		mappings = append(mappings, m)
	}
	return mappings, rows.Err()
}

func (s *Store) CreateMapping(m Mapping) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO mappings (org_id, grafana_team_name, grafana_team_id, external_group_id, external_group_name, role_override) VALUES (?, ?, ?, ?, ?, ?)`,
		m.OrgID, m.GrafanaTeamName, m.GrafanaTeamID, m.ExternalGroupID, m.ExternalGroupName, m.RoleOverride)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) DeleteMapping(id int64) error {
	_, err := s.db.Exec(`DELETE FROM mappings WHERE id = ?`, id)
	return err
}

func (s *Store) DeleteMappingsNotInGroupIDs(groupIDs []string) (int64, error) {
	if len(groupIDs) == 0 {
		return 0, nil
	}
	placeholders := make([]string, 0, len(groupIDs))
	args := make([]any, 0, len(groupIDs))
	for _, id := range groupIDs {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	query := fmt.Sprintf(`DELETE FROM mappings WHERE external_group_id NOT IN (%s)`, strings.Join(placeholders, ","))
	res, err := s.db.Exec(query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) UpdateMappingTeamID(id int64, teamID int64) error {
	_, err := s.db.Exec(`UPDATE mappings SET grafana_team_id = ?, updated_at = ? WHERE id = ?`, teamID, time.Now().UTC().Format(time.RFC3339), id)
	return err
}

func (s *Store) UpdateMappingTeamIDForName(orgID int64, teamName string, teamID int64) error {
	_, err := s.db.Exec(`UPDATE mappings SET grafana_team_id = ?, updated_at = ? WHERE org_id = ? AND grafana_team_name = ?`, teamID, time.Now().UTC().Format(time.RFC3339), orgID, teamName)
	return err
}

func (s *Store) ReplacePlan(plan Plan) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM plan_actions`); err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM plans`); err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	res, err := tx.Exec(`INSERT INTO plans (created_at, status) VALUES (?, ?)`, plan.CreatedAt, plan.Status)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	planID, err := res.LastInsertId()
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	stmt, err := tx.Prepare(`INSERT INTO plan_actions (plan_id, action_type, org_id, grafana_org_id, team_id, team_name, user_id, email, display_name, role, external_group_id, note) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	defer stmt.Close()
	for _, action := range plan.Actions {
		if _, err := stmt.Exec(planID, action.ActionType, action.OrgID, action.GrafanaOrgID, action.TeamID, action.TeamName, action.UserID, action.Email, action.DisplayName, action.Role, action.ExternalGroupID, action.Note); err != nil {
			_ = tx.Rollback()
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return planID, nil
}

func (s *Store) ClearPlan() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM plan_actions`); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM plans`); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) LatestPlan() (*Plan, error) {
	row := s.db.QueryRow(`SELECT id, created_at, status FROM plans ORDER BY id DESC LIMIT 1`)
	var plan Plan
	if err := row.Scan(&plan.ID, &plan.CreatedAt, &plan.Status); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	rows, err := s.db.Query(`SELECT id, plan_id, action_type, org_id, grafana_org_id, team_id, team_name, user_id, email, display_name, role, external_group_id, note FROM plan_actions WHERE plan_id = ? ORDER BY id`, plan.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var action PlanAction
		if err := rows.Scan(&action.ID, &action.PlanID, &action.ActionType, &action.OrgID, &action.GrafanaOrgID, &action.TeamID, &action.TeamName, &action.UserID, &action.Email, &action.DisplayName, &action.Role, &action.ExternalGroupID, &action.Note); err != nil {
			return nil, err
		}
		plan.Actions = append(plan.Actions, action)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &plan, nil
}

func (s *Store) UpdatePlanStatus(planID int64, status string) error {
	_, err := s.db.Exec(`UPDATE plans SET status = ? WHERE id = ?`, status, planID)
	return err
}

func migrate(db *sql.DB) error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS orgs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			grafana_org_id INTEGER NOT NULL UNIQUE,
			name TEXT,
			default_role TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS mappings (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			org_id INTEGER NOT NULL,
			grafana_team_name TEXT NOT NULL,
			grafana_team_id INTEGER NOT NULL DEFAULT 0,
			external_group_id TEXT NOT NULL,
			external_group_name TEXT,
			role_override TEXT,
			updated_at TEXT,
			FOREIGN KEY(org_id) REFERENCES orgs(id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_mappings_org_id ON mappings(org_id)`,
		`CREATE TABLE IF NOT EXISTS plans (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at TEXT NOT NULL,
			status TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS plan_actions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			plan_id INTEGER NOT NULL,
			action_type TEXT NOT NULL,
			org_id INTEGER,
			grafana_org_id INTEGER,
			team_id INTEGER,
			team_name TEXT,
			user_id INTEGER,
			email TEXT,
			display_name TEXT,
			role TEXT,
			external_group_id TEXT,
			note TEXT,
			FOREIGN KEY(plan_id) REFERENCES plans(id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_plan_actions_plan_id ON plan_actions(plan_id)`,
	}
	for _, q := range queries {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}
