package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/anateus/cicerone/internal/domain"
)

// PackageGroups returns every group, built-in Starred and Hidden included,
// ordered by position, with the number of packages assigned to each.
func (s *Store) PackageGroups(ctx context.Context) ([]domain.PackageGroup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT g.id, g.name, g.position, g.kind,
			(SELECT count(*) FROM packages p WHERE p.group_id = g.id)
		FROM package_groups g ORDER BY g.position, g.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := make([]domain.PackageGroup, 0)
	for rows.Next() {
		var group domain.PackageGroup
		if err := rows.Scan(&group.ID, &group.Name, &group.Index, &group.Kind, &group.Members); err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

// BuiltinGroup returns the Starred or Hidden group.
func (s *Store) BuiltinGroup(ctx context.Context, kind domain.GroupKind) (domain.PackageGroup, error) {
	groups, err := s.PackageGroups(ctx)
	if err != nil {
		return domain.PackageGroup{}, err
	}
	for _, group := range groups {
		if group.Kind == kind {
			return group, nil
		}
	}
	return domain.PackageGroup{}, fmt.Errorf("no built-in %s group", kind)
}

// CreatePackageGroup appends a new user group at the end of the ordering.
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
		result, err := tx.ExecContext(ctx, `INSERT INTO package_groups(name, position, kind) VALUES (?, ?, 'user')`, name, position)
		if err != nil {
			return err
		}
		id, err := result.LastInsertId()
		if err != nil {
			return err
		}
		group = domain.PackageGroup{ID: domain.PackageGroupID(id), Name: name, Index: int(position), Kind: domain.GroupKindUser}
		return nil
	})
	return group, err
}

// DeletePackageGroup removes a user group. Member packages return to
// ungrouped. The built-in Starred and Hidden groups cannot be deleted.
func (s *Store) DeletePackageGroup(ctx context.Context, id domain.PackageGroupID) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		var kind domain.GroupKind
		if err := tx.QueryRowContext(ctx, `SELECT kind FROM package_groups WHERE id=?`, id).Scan(&kind); err != nil {
			if err == sql.ErrNoRows {
				return nil
			}
			return err
		}
		if kind.Builtin() {
			return fmt.Errorf("the built-in group cannot be removed")
		}
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
	return s.SetPackagesGroup(ctx, []domain.PackageID{packageID}, groupID)
}

// SetPackagesGroup assigns several packages to one group, or clears their
// membership when the group ID is zero, in a single transaction.
func (s *Store) SetPackagesGroup(ctx context.Context, packageIDs []domain.PackageID, groupID domain.PackageGroupID) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		var value any
		if groupID != 0 {
			value = groupID
		}
		for _, packageID := range packageIDs {
			if _, err := tx.ExecContext(ctx, `UPDATE packages SET group_id=? WHERE id=?`, value, packageID); err != nil {
				return err
			}
		}
		return nil
	})
}
