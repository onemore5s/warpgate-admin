package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/onemore5s/warpgate-admin/internal/inventory"
	"github.com/onemore5s/warpgate-admin/internal/warpgate"
)

type API interface {
	ListGroups(context.Context) ([]warpgate.TargetGroup, error)
	CreateGroup(context.Context, warpgate.GroupRequest) (warpgate.TargetGroup, error)
	UpdateGroup(context.Context, string, warpgate.GroupRequest) (warpgate.TargetGroup, error)
	ListTargets(context.Context) ([]warpgate.Target, error)
	CreateTarget(context.Context, warpgate.TargetRequest) (warpgate.Target, error)
	UpdateTarget(context.Context, string, warpgate.TargetRequest) (warpgate.Target, error)
	DeleteTarget(context.Context, string) error
	ListRoles(context.Context) ([]warpgate.Role, error)
	ListTargetRoles(context.Context, string) ([]warpgate.Role, error)
	AddTargetRole(context.Context, string, string) error
	RemoveTargetRole(context.Context, string, string) error
}

type Action string

const (
	ActionCreate    Action = "create"
	ActionUpdate    Action = "update"
	ActionDelete    Action = "delete"
	ActionUnchanged Action = "unchanged"
)

type Summary struct {
	Create    int
	Update    int
	Delete    int
	Unchanged int
}

type groupChange struct {
	action  Action
	desired inventory.Group
	current *warpgate.TargetGroup
}

type targetChange struct {
	action  Action
	desired inventory.Target
	current *warpgate.Target
}

type targetRoleChange struct {
	action     Action
	targetName string
	role       warpgate.Role
}

type Plan struct {
	groups      []groupChange
	targets     []targetChange
	targetRoles []targetRoleChange
}

func (plan Plan) Summary() Summary {
	var result Summary
	count := func(action Action) {
		switch action {
		case ActionCreate:
			result.Create++
		case ActionUpdate:
			result.Update++
		case ActionDelete:
			result.Delete++
		case ActionUnchanged:
			result.Unchanged++
		}
	}
	for _, change := range plan.groups {
		count(change.action)
	}
	for _, change := range plan.targets {
		count(change.action)
	}
	for _, change := range plan.targetRoles {
		count(change.action)
	}
	return result
}

func (plan Plan) String() string {
	var output strings.Builder
	write := func(action Action, kind, name string) {
		if action == ActionUnchanged {
			return
		}
		prefix := "+"
		switch action {
		case ActionUpdate:
			prefix = "~"
		case ActionDelete:
			prefix = "-"
		}
		fmt.Fprintf(&output, "%s %s %s\n", prefix, kind, name)
	}
	for _, change := range plan.groups {
		write(change.action, "group", change.desired.Name)
	}
	for _, change := range plan.targets {
		write(change.action, "target", change.desired.Name)
	}
	for _, change := range plan.targetRoles {
		write(change.action, "role", change.role.Name+" -> target "+change.targetName)
	}
	summary := plan.Summary()
	fmt.Fprintf(&output, "\nPlan: %d to create, %d to update, %d to delete, %d unchanged.\n", summary.Create, summary.Update, summary.Delete, summary.Unchanged)
	return output.String()
}

type Reconciler struct {
	api             API
	updatePasswords bool
	syncRoles       bool
	syncTargets     bool
}

type Option func(*Reconciler)

func WithPasswordUpdates() Option {
	return func(reconciler *Reconciler) {
		reconciler.updatePasswords = true
	}
}

func WithRoleSync() Option {
	return func(reconciler *Reconciler) {
		reconciler.syncRoles = true
	}
}

func WithTargetSync() Option {
	return func(reconciler *Reconciler) {
		reconciler.syncTargets = true
	}
}

func New(api API, options ...Option) *Reconciler {
	reconciler := &Reconciler{api: api}
	for _, option := range options {
		option(reconciler)
	}
	return reconciler
}

func (reconciler *Reconciler) Plan(ctx context.Context, desired inventory.Inventory) (Plan, error) {
	groups, err := reconciler.api.ListGroups(ctx)
	if err != nil {
		return Plan{}, fmt.Errorf("list target groups: %w", err)
	}
	targets, err := reconciler.api.ListTargets(ctx)
	if err != nil {
		return Plan{}, fmt.Errorf("list targets: %w", err)
	}

	groupsByName, err := uniqueGroups(groups)
	if err != nil {
		return Plan{}, err
	}
	targetsByName, err := uniqueTargets(targets)
	if err != nil {
		return Plan{}, err
	}
	rolesByName := map[string]*warpgate.Role{}
	if inventoryHasRoles(desired) {
		roles, listErr := reconciler.api.ListRoles(ctx)
		if listErr != nil {
			return Plan{}, fmt.Errorf("list roles: %w", listErr)
		}
		rolesByName, err = uniqueRoles(roles)
		if err != nil {
			return Plan{}, err
		}
	}

	desired.Groups = append([]inventory.Group(nil), desired.Groups...)
	desired.Targets = append([]inventory.Target(nil), desired.Targets...)
	sort.Slice(desired.Groups, func(i, j int) bool { return desired.Groups[i].Name < desired.Groups[j].Name })
	sort.Slice(desired.Targets, func(i, j int) bool { return desired.Targets[i].Name < desired.Targets[j].Name })

	plan := Plan{}
	for _, group := range desired.Groups {
		current := groupsByName[group.Name]
		action := ActionCreate
		if current != nil {
			action = ActionUnchanged
			if !groupMatches(group, *current) {
				action = ActionUpdate
			}
		}
		plan.groups = append(plan.groups, groupChange{action: action, desired: group, current: current})
	}

	for _, target := range desired.Targets {
		if _, optionsErr := databaseOptions(target); optionsErr != nil {
			return Plan{}, fmt.Errorf("target %q: %w", target.Name, optionsErr)
		}
		current := targetsByName[target.Name]
		action := ActionCreate
		if current != nil {
			groupID := ""
			groupExists := false
			if group := groupsByName[target.Group]; group != nil {
				groupID = group.ID
				groupExists = true
			}
			matches, matchErr := targetMatches(target, groupID, *current)
			if matchErr != nil {
				return Plan{}, fmt.Errorf("target %q: %w", target.Name, matchErr)
			}
			action = ActionUpdate
			if groupExists && matches && !reconciler.updatePasswords {
				action = ActionUnchanged
			}
		}
		plan.targets = append(plan.targets, targetChange{action: action, desired: target, current: current})

		requestedRoles := append([]string(nil), target.Roles...)
		sort.Strings(requestedRoles)
		rolesManaged := target.RolesSpecified || len(requestedRoles) > 0
		currentRolesByName := map[string]*warpgate.Role{}
		if current != nil && (len(requestedRoles) > 0 || reconciler.syncRoles && rolesManaged) {
			currentRoles, listErr := reconciler.api.ListTargetRoles(ctx, current.ID)
			if listErr != nil {
				return Plan{}, fmt.Errorf("list roles for target %q: %w", target.Name, listErr)
			}
			currentRolesByName, err = uniqueRoles(currentRoles)
			if err != nil {
				return Plan{}, fmt.Errorf("target %q: %w", target.Name, err)
			}
		}
		seenRoles := make(map[string]struct{}, len(requestedRoles))
		for _, roleName := range requestedRoles {
			if _, exists := seenRoles[roleName]; exists {
				return Plan{}, fmt.Errorf("target %q: duplicate desired role %q", target.Name, roleName)
			}
			seenRoles[roleName] = struct{}{}
			role := rolesByName[roleName]
			if role == nil {
				return Plan{}, fmt.Errorf("target %q: unknown role %q", target.Name, roleName)
			}
			action := ActionCreate
			if currentRolesByName[roleName] != nil {
				action = ActionUnchanged
			}
			plan.targetRoles = append(plan.targetRoles, targetRoleChange{
				action:     action,
				targetName: target.Name,
				role:       *role,
			})
		}
		if reconciler.syncRoles && rolesManaged {
			currentRoleNames := make([]string, 0, len(currentRolesByName))
			for roleName := range currentRolesByName {
				currentRoleNames = append(currentRoleNames, roleName)
			}
			sort.Strings(currentRoleNames)
			for _, roleName := range currentRoleNames {
				if _, wanted := seenRoles[roleName]; wanted {
					continue
				}
				plan.targetRoles = append(plan.targetRoles, targetRoleChange{
					action:     ActionDelete,
					targetName: target.Name,
					role:       *currentRolesByName[roleName],
				})
			}
		}
	}

	if reconciler.syncTargets {
		managedGroupIDs := make(map[string]struct{}, len(desired.Groups))
		for _, group := range desired.Groups {
			if current := groupsByName[group.Name]; current != nil {
				managedGroupIDs[current.ID] = struct{}{}
			}
		}
		desiredTargetNames := make(map[string]struct{}, len(desired.Targets))
		for _, target := range desired.Targets {
			desiredTargetNames[target.Name] = struct{}{}
		}
		currentTargets := append([]warpgate.Target(nil), targets...)
		sort.Slice(currentTargets, func(i, j int) bool { return currentTargets[i].Name < currentTargets[j].Name })
		for index := range currentTargets {
			current := &currentTargets[index]
			if _, exists := desiredTargetNames[current.Name]; exists {
				continue
			}
			if _, managed := managedGroupIDs[current.GroupID]; !managed {
				continue
			}
			managed, classifyErr := isManagedRDSTarget(*current)
			if classifyErr != nil {
				return Plan{}, fmt.Errorf("classify target %q: %w", current.Name, classifyErr)
			}
			if !managed {
				continue
			}
			plan.targets = append(plan.targets, targetChange{
				action:  ActionDelete,
				desired: inventory.Target{Name: current.Name},
				current: current,
			})
		}
	}

	return plan, nil
}

func (reconciler *Reconciler) Apply(ctx context.Context, desired inventory.Inventory) (Plan, error) {
	plan, err := reconciler.Plan(ctx, desired)
	if err != nil {
		return Plan{}, err
	}

	groupIDs := make(map[string]string, len(plan.groups))
	for _, change := range plan.groups {
		request := groupRequest(change.desired)
		switch change.action {
		case ActionCreate:
			created, createErr := reconciler.api.CreateGroup(ctx, request)
			if createErr != nil {
				return plan, fmt.Errorf("create group %q: %w", change.desired.Name, createErr)
			}
			groupIDs[change.desired.Name] = created.ID
		case ActionUpdate:
			updated, updateErr := reconciler.api.UpdateGroup(ctx, change.current.ID, request)
			if updateErr != nil {
				return plan, fmt.Errorf("update group %q: %w", change.desired.Name, updateErr)
			}
			groupIDs[change.desired.Name] = updated.ID
		case ActionUnchanged:
			groupIDs[change.desired.Name] = change.current.ID
		}
	}

	targetIDs := make(map[string]string, len(plan.targets))
	for _, change := range plan.targets {
		if change.action == ActionDelete {
			continue
		}
		groupID := groupIDs[change.desired.Group]
		request, requestErr := targetRequest(change.desired, groupID, change.current)
		if requestErr != nil {
			return plan, fmt.Errorf("build target %q request: %w", change.desired.Name, requestErr)
		}
		switch change.action {
		case ActionCreate:
			created, createErr := reconciler.api.CreateTarget(ctx, request)
			if createErr != nil {
				return plan, fmt.Errorf("create target %q: %w", change.desired.Name, createErr)
			}
			targetIDs[change.desired.Name] = created.ID
		case ActionUpdate:
			if _, updateErr := reconciler.api.UpdateTarget(ctx, change.current.ID, request); updateErr != nil {
				return plan, fmt.Errorf("update target %q: %w", change.desired.Name, updateErr)
			}
			targetIDs[change.desired.Name] = change.current.ID
		case ActionUnchanged:
			targetIDs[change.desired.Name] = change.current.ID
		}
	}

	for _, change := range plan.targetRoles {
		switch change.action {
		case ActionCreate:
			if addErr := reconciler.api.AddTargetRole(ctx, targetIDs[change.targetName], change.role.ID); addErr != nil {
				return plan, fmt.Errorf("add role %q to target %q: %w", change.role.Name, change.targetName, addErr)
			}
		case ActionDelete:
			if removeErr := reconciler.api.RemoveTargetRole(ctx, targetIDs[change.targetName], change.role.ID); removeErr != nil {
				return plan, fmt.Errorf("remove role %q from target %q: %w", change.role.Name, change.targetName, removeErr)
			}
		}
	}

	for _, change := range plan.targets {
		if change.action != ActionDelete {
			continue
		}
		if deleteErr := reconciler.api.DeleteTarget(ctx, change.current.ID); deleteErr != nil {
			return plan, fmt.Errorf("delete target %q: %w", change.desired.Name, deleteErr)
		}
	}

	return plan, nil
}

func uniqueGroups(groups []warpgate.TargetGroup) (map[string]*warpgate.TargetGroup, error) {
	result := make(map[string]*warpgate.TargetGroup, len(groups))
	for index := range groups {
		group := &groups[index]
		if _, exists := result[group.Name]; exists {
			return nil, fmt.Errorf("Warpgate returned duplicate group name %q", group.Name)
		}
		result[group.Name] = group
	}
	return result, nil
}

func uniqueTargets(targets []warpgate.Target) (map[string]*warpgate.Target, error) {
	result := make(map[string]*warpgate.Target, len(targets))
	for index := range targets {
		target := &targets[index]
		if _, exists := result[target.Name]; exists {
			return nil, fmt.Errorf("Warpgate returned duplicate target name %q", target.Name)
		}
		result[target.Name] = target
	}
	return result, nil
}

func uniqueRoles(roles []warpgate.Role) (map[string]*warpgate.Role, error) {
	result := make(map[string]*warpgate.Role, len(roles))
	for index := range roles {
		role := &roles[index]
		if _, exists := result[role.Name]; exists {
			return nil, fmt.Errorf("Warpgate returned duplicate role name %q", role.Name)
		}
		result[role.Name] = role
	}
	return result, nil
}

func inventoryHasRoles(desired inventory.Inventory) bool {
	for _, target := range desired.Targets {
		if len(target.Roles) > 0 {
			return true
		}
	}
	return false
}

func isManagedRDSTarget(target warpgate.Target) (bool, error) {
	var metadata struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(target.Options, &metadata); err != nil {
		return false, fmt.Errorf("decode current options: %w", err)
	}
	return metadata.Kind == warpgate.MySQLKind || metadata.Kind == warpgate.PostgresKind, nil
}

func groupMatches(desired inventory.Group, current warpgate.TargetGroup) bool {
	return desired.Description == current.Description && desired.Color == stringValue(current.Color)
}

func targetMatches(desired inventory.Target, groupID string, current warpgate.Target) (bool, error) {
	var metadata struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(current.Options, &metadata); err != nil {
		return false, fmt.Errorf("decode current options: %w", err)
	}
	wanted, err := databaseOptions(desired)
	if err != nil {
		return false, err
	}
	var currentOptions any
	switch desired.Engine {
	case inventory.EngineMySQL:
		if metadata.Kind != warpgate.MySQLKind {
			return false, fmt.Errorf("existing target kind is %q, expected %q; refusing to change its kind", metadata.Kind, warpgate.MySQLKind)
		}
		var options warpgate.MySQLOptions
		if err := json.Unmarshal(current.Options, &options); err != nil {
			return false, fmt.Errorf("decode current MySQL options: %w", err)
		}
		if wantedOptions, ok := wanted.(warpgate.MySQLOptions); ok {
			clearComparablePasswords(&wantedOptions.Auth, &options.Auth, options.LegacyPassword)
			wanted = wantedOptions
			options.LegacyPassword = ""
		}
		currentOptions = options
	case inventory.EnginePostgres:
		if metadata.Kind != warpgate.PostgresKind {
			return false, fmt.Errorf("existing target kind is %q, expected %q; refusing to change its kind", metadata.Kind, warpgate.PostgresKind)
		}
		var options warpgate.PostgresOptions
		if err := json.Unmarshal(current.Options, &options); err != nil {
			return false, fmt.Errorf("decode current PostgreSQL options: %w", err)
		}
		if wantedOptions, ok := wanted.(warpgate.PostgresOptions); ok {
			clearComparablePasswords(&wantedOptions.Auth, &options.Auth, options.LegacyPassword)
			wantedOptions.IdleTimeout = options.IdleTimeout
			wanted = wantedOptions
			options.LegacyPassword = ""
		}
		currentOptions = options
	}
	return desired.Description == current.Description && groupID == current.GroupID && reflect.DeepEqual(wanted, currentOptions), nil
}

func groupRequest(group inventory.Group) warpgate.GroupRequest {
	var color *string
	if group.Color != "" {
		value := group.Color
		color = &value
	}
	return warpgate.GroupRequest{Name: group.Name, Description: group.Description, Color: color}
}

func targetRequest(target inventory.Target, groupID string, current *warpgate.Target) (warpgate.TargetRequest, error) {
	options, err := databaseOptions(target)
	if err != nil {
		return warpgate.TargetRequest{}, err
	}
	request := warpgate.TargetRequest{
		Name:        target.Name,
		Description: target.Description,
		GroupID:     groupID,
		Options:     options,
	}
	if current != nil {
		if postgres, ok := request.Options.(warpgate.PostgresOptions); ok {
			var currentOptions warpgate.PostgresOptions
			if json.Unmarshal(current.Options, &currentOptions) == nil {
				postgres.IdleTimeout = currentOptions.IdleTimeout
				request.Options = postgres
			}
		}
		request.RateLimitBytesPerSecond = current.RateLimitBytesPerSecond
		request.TicketMaxDurationSeconds = current.TicketMaxDurationSeconds
		request.TicketRequestsDisabled = current.TicketRequestsDisabled
		request.TicketRequireApproval = current.TicketRequireApproval
		request.TicketMaxUses = current.TicketMaxUses
	}
	return request, nil
}

func databaseOptions(target inventory.Target) (any, error) {
	tls := warpgate.TLS{Mode: string(target.TLSMode), Verify: target.TLSVerify}
	switch target.Engine {
	case inventory.EngineMySQL:
		return warpgate.MySQLOptions{
			Kind: warpgate.MySQLKind, Host: target.Host, Port: target.Port,
			Username: target.Username, Auth: passwordAuth(target.Password), TLS: tls,
			DefaultDatabaseName: target.DefaultDatabaseName,
		}, nil
	case inventory.EnginePostgres:
		return warpgate.PostgresOptions{
			Kind: warpgate.PostgresKind, Host: target.Host, Port: target.Port,
			Username: target.Username, Auth: passwordAuth(target.Password), TLS: tls,
			DefaultDatabaseName: target.DefaultDatabaseName, ProtocolVersion: target.ProtocolVersion,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported RDS engine %q", target.Engine)
	}
}

func passwordAuth(password string) *warpgate.DatabaseAuth {
	return &warpgate.DatabaseAuth{Kind: warpgate.PasswordAuthKind, Password: password}
}

func clearComparablePasswords(desired, current **warpgate.DatabaseAuth, legacyCurrent string) {
	currentPassword := legacyCurrent
	if *current != nil {
		currentPassword = (*current).Password
	} else {
		*current = passwordAuth(currentPassword)
	}
	if currentPassword == "" || strings.HasPrefix(currentPassword, "wgenc:v1:") {
		(*desired).Password = ""
		(*current).Password = ""
	}
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
