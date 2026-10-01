package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/anateus/cicerone/internal/domain"
)

// PackageGroups returns every user-defined group ordered by position.
func (s *Store) PackageGroups(ctx context.Context) ([]domain.PackageGroup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, position FROM package_groups ORDER BY position, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := make([]domain.PackageGroup, 0)
	for rows.Next() {
		var group domain.PackageGroup
		if err := rows.Scan(&group.ID, &group.Name, &group.Index); err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

// CreatePackageGroup appends a new group at the end of the ordering.
func (s *Store) CreatePackageGroup(ctx context.Context, name string) (domain.PackageGroup, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return domain.PackageGroup{}, fmt.Errorf("group name cannot be empty")
	}
	var group domain.PackageGroup
	err := s.Write(ctx, func(tx *sql.Tx) error {
		var maxPosition sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(position) FROM package_groups`).Scan(&maxPosition); err != nil {
			return err
		}
		position := maxPosition.Int64 + 1
		result, err := tx.ExecContext(ctx, `INSERT INTO package_groups(name, position) VALUES (?, ?)`, name, position)
		if err != nil {
			return err
		}
		id, err := result.LastInsertId()
		if err != nil {
			return err
		}
		group = domain.PackageGroup{ID: domain.PackageGroupID(id), Name: name, Index: int(position)}
		return nil
	})
	return group, err
}

// DeletePackageGroup removes a group. Member packages return to ungrouped.
func (s *Store) DeletePackageGroup(ctx context.Context, id domain.PackageGroupID) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE packages SET group_id=NULL WHERE group_id=?`, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM package_groups WHERE id=?`, id)
		return err
	})
}

// SetPackageGroup assigns a package to a group, or clears membership when the
// group ID is zero.
func (s *Store) SetPackageGroup(ctx context.Context, packageID domain.PackageID, groupID domain.PackageGroupID) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		if groupID == 0 {
			_, err := tx.ExecContext(ctx, `UPDATE packages SET group_id=NULL WHERE id=?`, packageID)
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE packages SET group_id=? WHERE id=?`, groupID, packageID)
		return err
	})
}
