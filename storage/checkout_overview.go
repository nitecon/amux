package storage

import (
	"context"
	"sort"
	"time"
)

// CheckoutOverview is the machine checkout view, separate from the stable
// /v1/projects inventory response.
type CheckoutOverview struct {
	Ident            string            `json:"ident"`
	CanonicalRemote  *string           `json:"canonical_remote"`
	Machines         []MachineCheckout `json:"machines"`
	UnavailableHosts []string          `json:"unavailable_hosts,omitempty"`
	Agents           int64             `json:"agents"`
	Active           int64             `json:"active"`
}

func (s *Store) CheckoutOverviews(ctx context.Context, now time.Time) ([]CheckoutOverview, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	projects, err := readProjects(ctx, tx, true)
	if err != nil {
		return nil, err
	}
	agents, _, err := livePresenceSnapshot(ctx, tx, now.UnixMilli())
	if err != nil {
		return nil, err
	}
	type counts struct{ agents, active int64 }
	agentCounts := map[string]counts{}
	for _, agent := range agents {
		count := agentCounts[agent.ProjectIdent]
		count.agents++
		if agent.State == "busy" {
			count.active++
		}
		agentCounts[agent.ProjectIdent] = count
	}
	stmt, err := tx.PrepareContext(ctx, "SELECT hostname,origin,unavailable FROM amux_go_machine_checkouts WHERE project_ident=? ORDER BY hostname")
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	result := make([]CheckoutOverview, 0, len(projects))
	for _, project := range projects {
		if project.ArchivedAt != nil {
			continue
		}
		rows, err := stmt.QueryContext(ctx, project.Ident)
		if err != nil {
			return nil, err
		}
		machines := make([]MachineCheckout, 0)
		var unavailableHosts []string
		for rows.Next() {
			var checkout MachineCheckout
			var unavailable bool
			if err := rows.Scan(&checkout.Hostname, &checkout.Origin, &unavailable); err != nil {
				rows.Close()
				return nil, err
			}
			if unavailable {
				unavailableHosts = append(unavailableHosts, checkout.Hostname)
				continue
			}
			checkout.Origin = safeCheckoutOrigin(checkout.Origin)
			machines = append(machines, checkout)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		count := agentCounts[project.Ident]
		result = append(result, CheckoutOverview{Ident: project.Ident, CanonicalRemote: project.CanonicalRemote, Machines: machines, UnavailableHosts: unavailableHosts, Agents: count.agents, Active: count.active})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Ident < result[j].Ident })
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}
