package idpsync

import (
	"context"
	"regexp"

	"github.com/golang-jwt/jwt/v4"
	"github.com/google/uuid"
	"golang.org/x/xerrors"

	"cdr.dev/slog"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/db2sdk"
	"github.com/coder/coder/v2/coderd/database/dbauthz"
	"github.com/coder/coder/v2/coderd/runtimeconfig"
	"github.com/coder/coder/v2/coderd/util/slice"
)

type GroupParams struct {
	// SyncEnabled if false will skip syncing the user's groups
	SyncEnabled  bool
	MergedClaims jwt.MapClaims
}

func (AGPLIDPSync) GroupSyncEnabled() bool {
	// AGPL does not support syncing groups.
	return false
}

func (s AGPLIDPSync) ParseGroupClaims(_ context.Context, _ jwt.MapClaims) (GroupParams, *HTTPError) {
	return GroupParams{
		SyncEnabled: s.GroupSyncEnabled(),
	}, nil
}

// TODO: Group allowlist behavior should probably happen at this step.
func (s AGPLIDPSync) SyncGroups(ctx context.Context, db database.Store, user database.User, params GroupParams) error {
	// Nothing happens if sync is not enabled
	if !params.SyncEnabled {
		return nil
	}

	// nolint:gocritic // all syncing is done as a system user
	ctx = dbauthz.AsSystemRestricted(ctx)

	db.InTx(func(tx database.Store) error {
		resolver := runtimeconfig.NewStoreResolver(tx)
		userGroups, err := tx.GetGroups(ctx, database.GetGroupsParams{
			HasMemberID: user.ID,
		})
		if err != nil {
			return xerrors.Errorf("get user groups: %w", err)
		}

		// Figure out which organizations the user is a member of.
		userOrgs := make(map[uuid.UUID][]database.GetGroupsRow)
		for _, g := range userGroups {
			g := g
			userOrgs[g.Group.OrganizationID] = append(userOrgs[g.Group.OrganizationID], g)
		}

		// For each org, we need to fetch the sync settings
		orgSettings := make(map[uuid.UUID]GroupSyncSettings)
		for orgID := range userOrgs {
			orgResolver := runtimeconfig.NewOrgResolver(orgID, resolver)
			settings, err := s.SyncSettings.Group.Resolve(ctx, orgResolver)
			if err != nil {
				return xerrors.Errorf("resolve group sync settings: %w", err)
			}
			orgSettings[orgID] = settings.Value
		}

		// collect all diffs to do 1 sql update for all orgs
		groupsToAdd := make([]uuid.UUID, 0)
		groupsToRemove := make([]uuid.UUID, 0)
		// For each org, determine which groups the user should land in
		for orgID, settings := range orgSettings {
			if settings.GroupField == "" {
				// No group sync enabled for this org, so do nothing.
				continue
			}

			expectedGroups, err := settings.ParseClaims(params.MergedClaims)
			if err != nil {
				s.Logger.Debug(ctx, "failed to parse claims for groups",
					slog.F("organization_field", s.GroupField),
					slog.F("organization_id", orgID),
					slog.Error(err),
				)
				// Unsure where to raise this error on the UI or database.
				continue
			}
			// Everyone group is always implied.
			expectedGroups = append(expectedGroups, ExpectedGroup{
				GroupID: &orgID,
			})

			// Now we know what groups the user should be in for a given org,
			// determine if we have to do any group updates to sync the user's
			// state.
			existingGroups := userOrgs[orgID]
			existingGroupsTyped := db2sdk.List(existingGroups, func(f database.GetGroupsRow) ExpectedGroup {
				return ExpectedGroup{
					GroupID:   &f.Group.ID,
					GroupName: &f.Group.Name,
				}
			})
			add, remove := slice.SymmetricDifferenceFunc(existingGroupsTyped, expectedGroups, func(a, b ExpectedGroup) bool {
				// Only the name or the name needs to be checked, priority is given to the ID.
				if a.GroupID != nil && b.GroupID != nil {
					return *a.GroupID == *b.GroupID
				}
				if a.GroupName != nil && b.GroupName != nil {
					return *a.GroupName == *b.GroupName
				}
				return false
			})

			// HandleMissingGroups will add the new groups to the org if
			// the settings specify. It will convert all group names into uuids
			// for easier assignment.
			assignGroups, err := settings.HandleMissingGroups(ctx, tx, orgID, add)
			if err != nil {
				return xerrors.Errorf("handle missing groups: %w", err)
			}

			for _, removeGroup := range remove {
				// This should always be the case.
				// TODO: make sure this is always the case
				if removeGroup.GroupID != nil {
					groupsToRemove = append(groupsToRemove, *removeGroup.GroupID)
				}
			}

			groupsToAdd = append(groupsToAdd, assignGroups...)
		}

		tx.InsertUserGroupsByID(ctx, database.InsertUserGroupsByIDParams{
			UserID: user.ID,
			GroupIds:   groupsToAdd,
		})
		return nil
	}, nil)

	//
	//tx.InTx(func(tx database.Store) error {
	//	// When setting the user's groups, it's easier to just clear their groups and re-add them.
	//	// This ensures that the user's groups are always in sync with the auth provider.
	//	 err := tx.RemoveUserFromAllGroups(ctx, user.ID)
	//	if err != nil {
	//		return err
	//	}
	//
	//	for _, org := range userOrgs {
	//
	//	}
	//
	//	return nil
	//}, nil)

	return nil
}

type GroupSyncSettings struct {
	GroupField string `json:"field"`
	// GroupMapping maps from an OIDC group --> Coder group ID
	GroupMapping            map[string][]uuid.UUID `json:"mapping"`
	RegexFilter             *regexp.Regexp         `json:"regex_filter"`
	AutoCreateMissingGroups bool                   `json:"auto_create_missing_groups"`
}

type ExpectedGroup struct {
	GroupID   *uuid.UUID
	GroupName *string
}

// ParseClaims will take the merged claims from the IDP and return the groups
// the user is expected to be a member of. The expected group can either be a
// name or an ID.
// It is unfortunate we cannot use exclusively names or exclusively IDs.
// When configuring though, if a group is mapped from "A" -> "UUID 1234", and
// the group "UUID 1234" is renamed, we want to maintain the mapping.
// We have to keep names because group sync supports syncing groups by name if
// the external IDP group name matches the Coder one.
func (s GroupSyncSettings) ParseClaims(mergedClaims jwt.MapClaims) ([]ExpectedGroup, error) {
	groupsRaw, ok := mergedClaims[s.GroupField]
	if !ok {
		return []ExpectedGroup{}, nil
	}

	parsedGroups, err := ParseStringSliceClaim(groupsRaw)
	if err != nil {
		return nil, xerrors.Errorf("parse groups field, unexpected type %T: %w", groupsRaw, err)
	}

	groups := make([]ExpectedGroup, 0)
	for _, group := range parsedGroups {
		// Only allow through groups that pass the regex
		if s.RegexFilter != nil {
			if !s.RegexFilter.MatchString(group) {
				continue
			}
		}

		mappedGroupIDs, ok := s.GroupMapping[group]
		if ok {
			for _, gid := range mappedGroupIDs {
				gid := gid
				groups = append(groups, ExpectedGroup{GroupID: &gid})
			}
			continue
		}
		group := group
		groups = append(groups, ExpectedGroup{GroupName: &group})
	}

	return groups, nil
}

func (s GroupSyncSettings) HandleMissingGroups(ctx context.Context, tx database.Store, orgID uuid.UUID, add []ExpectedGroup) ([]uuid.UUID, error) {
	if !s.AutoCreateMissingGroups {
		// Remove all groups that are missing, they will not be created
		filter := make([]uuid.UUID, 0)
		for _, expected := range add {
			if expected.GroupID != nil {
				filter = append(filter, *expected.GroupID)
			}
		}

		return filter, nil
	}
	// All expected that are missing IDs means the group does not exist
	// in the database. Either remove them, or create them if auto create is
	// turned on.
	var missingGroups []string
	addIDs := make([]uuid.UUID, 0)

	for _, expected := range add {
		if expected.GroupID == nil && expected.GroupName != nil {
			missingGroups = append(missingGroups, *expected.GroupName)
		} else if expected.GroupID != nil {
			// Keep the IDs to sync the groups.
			addIDs = append(addIDs, *expected.GroupID)
		}
	}

	createdMissingGroups, err := tx.InsertMissingGroups(ctx, database.InsertMissingGroupsParams{
		OrganizationID: orgID,
		Source:         database.GroupSourceOidc,
		GroupNames:     missingGroups,
	})
	if err != nil {
		return nil, xerrors.Errorf("insert missing groups: %w", err)
	}
	for _, created := range createdMissingGroups {
		addIDs = append(addIDs, created.ID)
	}

	return addIDs, nil
}
