package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	appconfig "github.com/onemore5s/warpgate-admin/internal/config"
	"github.com/onemore5s/warpgate-admin/internal/inventory"
	"github.com/onemore5s/warpgate-admin/internal/reconcile"
	"github.com/onemore5s/warpgate-admin/internal/warpgate"
)

var version = "dev"

type dependencies struct {
	getenv   func(string) string
	openFile func(string) (io.ReadCloser, error)
	newAPI   func(warpgate.Config) (reconcile.API, error)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	deps := dependencies{
		getenv:   os.Getenv,
		openFile: func(name string) (io.ReadCloser, error) { return os.Open(name) },
		newAPI: func(config warpgate.Config) (reconcile.API, error) {
			return warpgate.NewClient(config)
		},
	}
	os.Exit(run(ctx, os.Args[1:], deps, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, deps dependencies, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}

	switch args[0] {
	case "help", "--help", "-h":
		printUsage(stdout)
		return 0
	case "version":
		fmt.Fprintln(stdout, version)
		return 0
	case "validate", "plan", "apply":
		return runInventoryCommand(ctx, args[0], args[1:], deps, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "Unknown command %q.\n\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func runInventoryCommand(ctx context.Context, command string, args []string, deps dependencies, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintf(stderr, "Usage: warpgate-admin %s --config <warpgate-admin.yaml>\n", command)
	}
	configFileName := flags.String("config", "", "YAML configuration file path")
	groupsFileName := flags.String("groups", "", "Target Group CSV path")
	targetsFileName := flags.String("targets", "", "RDS Target CSV path")
	updatePasswords := flags.Bool("update-passwords", false, "force password updates for all managed RDS targets")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "Positional arguments are not supported: %v.\n", flags.Args())
		return 2
	}

	var fileConfig appconfig.File
	if *configFileName != "" {
		configFile, openErr := deps.openFile(*configFileName)
		if openErr != nil {
			fmt.Fprintf(stderr, "Failed to open configuration file: %v.\n", openErr)
			return 1
		}
		parsed, parseErr := appconfig.Parse(configFile)
		_ = configFile.Close()
		if parseErr != nil {
			fmt.Fprintf(stderr, "Configuration validation failed: %v.\n", parseErr)
			return 1
		}
		fileConfig = parsed.ResolvePaths(*configFileName)
		if *groupsFileName == "" {
			*groupsFileName = fileConfig.Inventory.Groups
		}
		if *targetsFileName == "" {
			*targetsFileName = fileConfig.Inventory.Targets
		}
	}
	if *groupsFileName == "" {
		fmt.Fprintln(stderr, "Specify the groups inventory with either --config or --groups.")
		return 2
	}
	if *targetsFileName == "" {
		fmt.Fprintln(stderr, "Specify the targets inventory with either --config or --targets.")
		return 2
	}

	groupsFile, err := deps.openFile(*groupsFileName)
	if err != nil {
		fmt.Fprintf(stderr, "Failed to open groups inventory: %v.\n", err)
		return 1
	}
	defer groupsFile.Close()
	targetsFile, err := deps.openFile(*targetsFileName)
	if err != nil {
		fmt.Fprintf(stderr, "Failed to open targets inventory: %v.\n", err)
		return 1
	}
	defer targetsFile.Close()

	desired, err := inventory.Parse(groupsFile, targetsFile)
	if err != nil {
		fmt.Fprintf(stderr, "Inventory validation failed: %v.\n", err)
		return 1
	}
	if command == "validate" {
		fmt.Fprintf(stdout, "Inventory is valid: %d %s, %d %s.\n", len(desired.Groups), plural(len(desired.Groups), "group", "groups"), len(desired.Targets), plural(len(desired.Targets), "target", "targets"))
		return 0
	}

	clientConfig, err := configFromFile(fileConfig)
	if err != nil {
		fmt.Fprintf(stderr, "Configuration error: %v.\n", err)
		return 2
	}
	desired, err = inventory.ResolvePasswords(desired, deps.getenv)
	if err != nil {
		fmt.Fprintf(stderr, "Failed to resolve RDS passwords: %v.\n", err)
		return 2
	}
	api, err := deps.newAPI(clientConfig)
	if err != nil {
		fmt.Fprintf(stderr, "Failed to create Warpgate API client: %v.\n", err)
		return 1
	}
	var reconcileOptions []reconcile.Option
	if *updatePasswords || fileConfig.Apply.UpdatePasswords {
		reconcileOptions = append(reconcileOptions, reconcile.WithPasswordUpdates())
	}
	if fileConfig.Apply.SyncRoles {
		reconcileOptions = append(reconcileOptions, reconcile.WithRoleSync())
	}
	if fileConfig.Apply.SyncTargets {
		reconcileOptions = append(reconcileOptions, reconcile.WithTargetSync())
	}
	reconciler := reconcile.New(api, reconcileOptions...)

	if command == "plan" {
		plan, planErr := reconciler.Plan(ctx, desired)
		if planErr != nil {
			fmt.Fprintf(stderr, "Failed to build plan: %v.\n", planErr)
			return 1
		}
		fmt.Fprint(stdout, plan.String())
		return 0
	}

	plan, applyErr := reconciler.Apply(ctx, desired)
	if applyErr != nil {
		fmt.Fprintf(stderr, "Apply failed: %v.\n", applyErr)
		return 1
	}
	fmt.Fprint(stdout, plan.String())
	fmt.Fprintln(stdout, "Apply complete.")
	return 0
}

func configFromFile(fileConfig appconfig.File) (warpgate.Config, error) {
	if fileConfig.Warpgate.Host == "" {
		return warpgate.Config{}, fmt.Errorf("Warpgate host is not configured; set warpgate.host")
	}
	if fileConfig.Warpgate.Token == "" {
		return warpgate.Config{}, fmt.Errorf("Warpgate token is not configured; set warpgate.token")
	}
	return warpgate.Config{
		Host:               fileConfig.Warpgate.Host,
		Token:              fileConfig.Warpgate.Token,
		InsecureSkipVerify: fileConfig.Warpgate.InsecureSkipVerify,
	}, nil
}

func printUsage(output io.Writer) {
	fmt.Fprintln(output, `Usage:
  warpgate-admin validate --config <warpgate-admin.yaml>
  warpgate-admin plan     --config <warpgate-admin.yaml>
  warpgate-admin apply    --config <warpgate-admin.yaml>

Validate CSV files without YAML:
  warpgate-admin validate --groups <groups.csv> --targets <targets.csv>

Options:
  --config <path>                   YAML configuration file
  --groups <path>                   override the groups CSV path from YAML
  --targets <path>                  override the targets CSV path from YAML
  --update-passwords               force password updates for all managed RDS targets

RDS password environment variables:
  <password_env>                    variable referenced by targets.csv`)
}

func plural(count int, singular, pluralForm string) string {
	if count == 1 {
		return singular
	}
	return pluralForm
}
