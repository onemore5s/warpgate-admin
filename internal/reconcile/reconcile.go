package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
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

// Preserve earlier defaults for new targets; since 0.29.0, writes must include them explicitly.
const (
	defaultTicketRequestsDisabled = false
	defaultTicketRequireApproval  = false
	defaultRequireApproval        = false
)

// Match Warpgate's default when neither the inventory nor the existing target specifies a version.
const defaultPostgresProtocolVersion = "3.2"

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
	api                 API
	updatePasswords     bool
	syncRoles           bool
	syncDatabaseTargets bool
	syncHTTPTargets     bool
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

func WithDatabaseTargetSync() Option {
	return func(reconciler *Reconciler) {
		reconciler.syncDatabaseTargets = true
	}
}

func WithHTTPTargetSync() Option {
	return func(reconciler *Reconciler) { reconciler.syncHTTPTargets = true }
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

	groupsByName, err := uniqueByName(groups, func(group *warpgate.TargetGroup) string { return group.Name }, "group")
	if err != nil {
		return Plan{}, err
	}
	targetsByName, err := uniqueByName(targets, func(target *warpgate.Target) string { return target.Name }, "target")
	if err != nil {
		return Plan{}, err
	}
	rolesByName := map[string]*warpgate.Role{}
	if inventoryHasRoles(desired) {
		roles, listErr := reconciler.api.ListRoles(ctx)
		if listErr != nil {
			return Plan{}, fmt.Errorf("list roles: %w", listErr)
		}
		rolesByName, err = uniqueByName(roles, func(role *warpgate.Role) string { return role.Name }, "role")
		if err != nil {
			return Plan{}, err
		}
	}

	desired.Groups = append([]inventory.Group(nil), desired.Groups...)
	desired.Targets = append([]inventory.Target(nil), desired.Targets...)
	sortByName(desired.Groups, func(group inventory.Group) string { return group.Name })
	sortByName(desired.Targets, func(target inventory.Target) string { return target.Name })

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
		if _, optionsErr := targetOptions(target); optionsErr != nil {
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
			if groupExists && matches && !(reconciler.updatePasswords && target.Engine != inventory.EngineHTTP) {
				action = ActionUnchanged
			}
		}
		plan.targets = append(plan.targets, targetChange{action: action, desired: target, current: current})

		requestedRoles := append([]string(nil), target.Roles...)
		sort.Strings(requestedRoles)
		rolesManaged := target.RolesSpecified || len(requestedRoles) > 0
		currentRolesByName := map[string]*warpgate.Role{}
		if current != nil && (len(requestedRoles) > 0 || reconciler.syncRoles && target.RolesSpecified) {
			currentRoles, listErr := reconciler.api.ListTargetRoles(ctx, current.ID)
			if listErr != nil {
				return Plan{}, fmt.Errorf("list roles for target %q: %w", target.Name, listErr)
			}
			currentRolesByName, err = uniqueByName(currentRoles, func(role *warpgate.Role) string { return role.Name }, "role")
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

	if reconciler.syncDatabaseTargets || reconciler.syncHTTPTargets {
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
		sortByName(currentTargets, func(target warpgate.Target) string { return target.Name })
		for index := range currentTargets {
			current := &currentTargets[index]
			if _, exists := desiredTargetNames[current.Name]; exists {
				continue
			}
			if _, managed := managedGroupIDs[current.GroupID]; !managed {
				continue
			}
			managed, classifyErr := reconciler.isManagedTarget(*current)
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

func uniqueByName[T any](items []T, name func(*T) string, kind string) (map[string]*T, error) {
	result := make(map[string]*T, len(items))
	for index := range items {
		item := &items[index]
		key := name(item)
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("Warpgate returned duplicate %s name %q", kind, key)
		}
		result[key] = item
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

func (reconciler *Reconciler) isManagedTarget(target warpgate.Target) (bool, error) {
	var metadata struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(target.Options, &metadata); err != nil {
		return false, fmt.Errorf("decode current options: %w", err)
	}
	return (reconciler.syncDatabaseTargets && (metadata.Kind == warpgate.MySQLKind || metadata.Kind == warpgate.PostgresKind)) || (reconciler.syncHTTPTargets && metadata.Kind == warpgate.HTTPKind), nil
}

func groupMatches(desired inventory.Group, current warpgate.TargetGroup) bool {
	return desired.Description == current.Description && desired.Color == stringValue(current.Color)
}

func targetMatches(desired inventory.Target, groupID string, current warpgate.Target) (bool, error) {
	wanted, err := targetOptions(desired)
	if err != nil {
		return false, err
	}
	var currentOptions any
	switch desired.Engine {
	case inventory.EngineHTTP:
		var options warpgate.HTTPOptions
		if err := json.Unmarshal(current.Options, &options); err != nil {
			return false, fmt.Errorf("decode current HTTP options: %w", err)
		}
		if options.Kind != warpgate.HTTPKind {
			return false, fmt.Errorf("existing target kind is %q, expected %q; refusing to change its kind", options.Kind, warpgate.HTTPKind)
		}
		wanted = mergeHTTPOptions(desired, wanted.(warpgate.HTTPOptions), options)
		if options.Headers == nil {
			options.Headers = map[string]string{}
		}
		if options.ExternalHost != nil && *options.ExternalHost == "" {
			options.ExternalHost = nil
		}
		currentOptions = options
	case inventory.EngineMySQL:
		var options warpgate.MySQLOptions
		if err := json.Unmarshal(current.Options, &options); err != nil {
			return false, fmt.Errorf("decode current MySQL options: %w", err)
		}
		if options.Kind != warpgate.MySQLKind {
			return false, fmt.Errorf("existing target kind is %q, expected %q; refusing to change its kind", options.Kind, warpgate.MySQLKind)
		}
		if wantedOptions, ok := wanted.(warpgate.MySQLOptions); ok {
			wantedOptions.Auth, options.Auth = clearComparablePasswords(wantedOptions.Auth, options.Auth, options.LegacyPassword)
			wanted = wantedOptions
			options.LegacyPassword = ""
		}
		currentOptions = options
	case inventory.EnginePostgres:
		var options warpgate.PostgresOptions
		if err := json.Unmarshal(current.Options, &options); err != nil {
			return false, fmt.Errorf("decode current PostgreSQL options: %w", err)
		}
		if options.Kind != warpgate.PostgresKind {
			return false, fmt.Errorf("existing target kind is %q, expected %q; refusing to change its kind", options.Kind, warpgate.PostgresKind)
		}
		if wantedOptions, ok := wanted.(warpgate.PostgresOptions); ok {
			wantedOptions.Auth, options.Auth = clearComparablePasswords(wantedOptions.Auth, options.Auth, options.LegacyPassword)
			wantedOptions.ProtocolVersion = postgresProtocolVersion(desired.ProtocolVersion, options.ProtocolVersion)
			options.ProtocolVersion = postgresProtocolVersion(options.ProtocolVersion, "")
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
	options, err := targetOptions(target)
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
		if httpOptions, ok := request.Options.(warpgate.HTTPOptions); ok {
			var currentOptions warpgate.HTTPOptions
			if err := json.Unmarshal(current.Options, &currentOptions); err != nil {
				return warpgate.TargetRequest{}, fmt.Errorf("decode current HTTP options: %w", err)
			}
			request.Options = mergeHTTPOptions(target, httpOptions, currentOptions)
		}
		if postgres, ok := request.Options.(warpgate.PostgresOptions); ok {
			var currentOptions warpgate.PostgresOptions
			if json.Unmarshal(current.Options, &currentOptions) == nil {
				postgres.ProtocolVersion = postgresProtocolVersion(target.ProtocolVersion, currentOptions.ProtocolVersion)
				postgres.IdleTimeout = currentOptions.IdleTimeout
				request.Options = postgres
			}
		}
		request.RateLimitBytesPerSecond = current.RateLimitBytesPerSecond
		request.TicketMaxDurationSeconds = current.TicketMaxDurationSeconds
		request.TicketRequestsDisabled = current.TicketRequestsDisabled
		request.TicketRequireApproval = current.TicketRequireApproval
		request.RequireApproval = current.RequireApproval
		request.TicketMaxUses = current.TicketMaxUses
	}
	request.TicketRequestsDisabled = approvalPolicyValue(request.TicketRequestsDisabled, defaultTicketRequestsDisabled)
	request.TicketRequireApproval = approvalPolicyValue(request.TicketRequireApproval, defaultTicketRequireApproval)
	request.RequireApproval = approvalPolicyValue(request.RequireApproval, defaultRequireApproval)
	return request, nil
}

func approvalPolicyValue(value *bool, fallback bool) *bool {
	if value != nil {
		return value
	}
	return &fallback
}

func targetOptions(target inventory.Target) (any, error) {
	tls := warpgate.TLS{Mode: string(target.TLSMode), Verify: target.TLSVerify}
	switch target.Engine {
	case inventory.EngineHTTP:
		if target.HTTP == nil {
			return nil, fmt.Errorf("HTTP target options are required")
		}
		options := warpgate.HTTPOptions{Kind: warpgate.HTTPKind, URL: target.HTTP.URL, TLS: tls, Headers: target.HTTP.Headers, ExternalHost: target.HTTP.ExternalHost}
		return mergeHTTPOptions(target, options, warpgate.HTTPOptions{}), nil
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
			DefaultDatabaseName: target.DefaultDatabaseName, ProtocolVersion: postgresProtocolVersion(target.ProtocolVersion, ""),
		}, nil
	default:
		return nil, fmt.Errorf("unsupported RDS engine %q", target.Engine)
	}
}

func postgresProtocolVersion(configured, current string) string {
	if configured != "" {
		return configured
	}
	if current != "" {
		return current
	}
	return defaultPostgresProtocolVersion
}

func mergeHTTPOptions(target inventory.Target, wanted, current warpgate.HTTPOptions) warpgate.HTTPOptions {
	if !target.HTTP.HeadersSpecified {
		wanted.Headers = current.Headers
	}
	wanted.Headers = maps.Clone(wanted.Headers)
	if wanted.Headers == nil {
		wanted.Headers = map[string]string{}
	}
	if target.HTTP.ExternalHost == nil {
		wanted.ExternalHost = current.ExternalHost
	}
	if wanted.ExternalHost != nil && *wanted.ExternalHost == "" {
		wanted.ExternalHost = nil
	}
	return wanted
}

func passwordAuth(password string) *warpgate.DatabaseAuth {
	return &warpgate.DatabaseAuth{Kind: warpgate.PasswordAuthKind, Password: password}
}

// encryptedPasswordPrefix marks a password Warpgate has already encrypted server-side;
// such values are never comparable to the plaintext password in the inventory CSV.
const encryptedPasswordPrefix = "wgenc:v1:"

func clearComparablePasswords(desired, current *warpgate.DatabaseAuth, legacyCurrent string) (*warpgate.DatabaseAuth, *warpgate.DatabaseAuth) {
	currentPassword := legacyCurrent
	if current != nil {
		currentPassword = current.Password
	} else {
		current = passwordAuth(currentPassword)
	}
	if currentPassword == "" || strings.HasPrefix(currentPassword, encryptedPasswordPrefix) {
		desired.Password = ""
		current.Password = ""
	}
	return desired, current
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func sortByName[T any](items []T, name func(T) string) {
	sort.Slice(items, func(i, j int) bool { return name(items[i]) < name(items[j]) })
}
