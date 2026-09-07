package store

import (
	"context"
	"fmt"
)

// ArrInstance is one discovered arr/Prowlarr target recorded by the wizard.
type ArrInstance struct {
	Name       string
	URL        string
	AppType    string
	APIKeyRef  string
	Discovered string
	// RootFolder and QualityProfile are RD-34 D4's operator-captured
	// destination values. Both are required before automatic reactive
	// commit may ADD content to this instance; zero values read as NOT
	// CAPTURED and park, preserving the shipping default OFF.
	RootFolder     string
	QualityProfile int
}

// CanAdd reports whether automatic dispatch may ADD content here. RD-34 D4
// requires BOTH captured values; a partially configured instance stays
// manually routable but is never chosen automatically.
func (a ArrInstance) CanAdd() bool {
	return a.RootFolder != "" && a.QualityProfile > 0
}

// UpsertArrInstance records one discovered arr target for later S9-S11 steps.
func (s *Store) UpsertArrInstance(ctx context.Context, inst ArrInstance) error {
	_, err := s.execWrite(ctx, `
        INSERT INTO arr_instances (name, url, app_type, api_key_ref, discovered_at)
        VALUES (?, ?, ?, ?, ?)
        ON CONFLICT(name) DO UPDATE SET
            url=excluded.url,
            app_type=excluded.app_type,
            api_key_ref=excluded.api_key_ref,
            discovered_at=excluded.discovered_at`,
		inst.Name, inst.URL, inst.AppType, inst.APIKeyRef, formatTime(s.now()))
	if err != nil {
		return fmt.Errorf("upsert arr instance %s: %w", inst.Name, err)
	}
	return nil
}

// ListArrInstances returns the wizard-discovered arr targets in stable order.
func (s *Store) ListArrInstances(ctx context.Context) ([]ArrInstance, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT name, url, app_type, api_key_ref, discovered_at,
               root_folder, quality_profile
        FROM arr_instances
        ORDER BY app_type, name`)
	if err != nil {
		return nil, fmt.Errorf("list arr instances: %w", err)
	}
	defer rows.Close()

	var out []ArrInstance
	for rows.Next() {
		var inst ArrInstance
		if err := rows.Scan(&inst.Name, &inst.URL, &inst.AppType, &inst.APIKeyRef,
			&inst.Discovered, &inst.RootFolder, &inst.QualityProfile); err != nil {
			return nil, fmt.Errorf("scan arr instance: %w", err)
		}
		out = append(out, inst)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate arr instances: %w", err)
	}
	return out, nil
}

// SetArrInstanceDestination records the operator's captured root folder and
// quality profile for one instance. Rediscovery does NOT touch these columns,
// so re-running the wizard's discovery step cannot silently clear a
// destination the operator chose.
func (s *Store) SetArrInstanceDestination(ctx context.Context, name, rootFolder string, qualityProfile int) error {
	result, err := s.execWrite(ctx, `
        UPDATE arr_instances
        SET root_folder = ?, quality_profile = ?
        WHERE name = ?`, rootFolder, qualityProfile, name)
	if err != nil {
		return fmt.Errorf("set arr destination %s: %w", name, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("set arr destination %s: %w", name, err)
	}
	if affected == 0 {
		return fmt.Errorf("set arr destination %s: no such instance", name)
	}
	return nil
}
