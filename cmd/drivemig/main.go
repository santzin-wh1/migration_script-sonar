// Command drivemig migrates Google Drive contents between accounts.
//
// Subcommands:
//
//	drivemig auth <email>     acquire an OAuth refresh token (device flow) for a @gmail account
//	drivemig prepare ...      map the origin Drive, share items, write folders/manifest CSVs
//	drivemig apply ...        recreate folders and copy files into the destination
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/WH1-Cloud/migration_script/internal/apply"
	"github.com/WH1-Cloud/migration_script/internal/auth"
	"github.com/WH1-Cloud/migration_script/internal/logging"
	"github.com/WH1-Cloud/migration_script/internal/manifest"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "auth":
		err = cmdAuth(os.Args[2:])
	case "prepare":
		err = cmdPrepare(os.Args[2:])
	case "apply":
		err = cmdApply(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `drivemig — Google Drive migration (@gmail OAuth / Workspace DWD)

usage:
  drivemig auth <email>     [--project P] [--client-secret NAME] [--token-prefix PFX] [--timeout D]
  drivemig prepare --csv F  --project P [...]
  drivemig apply --folders F --manifest F [--project P] [--workers N] [...]

run "drivemig <subcommand> -h" for flags.
`)
}

// resolverFlags holds the credential flags shared by all subcommands.
type resolverFlags struct {
	saJSON       string
	clientSecret string
	tokenDir     string
	timeout      time.Duration
}

func (rf *resolverFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&rf.saJSON, "sa-json", "service_account.json", "service-account JSON for DWD (Workspace)")
	fs.StringVar(&rf.clientSecret, "client-secret", "client_secret.json", "OAuth client secret JSON file")
	fs.StringVar(&rf.tokenDir, "token-dir", "tokens", "directory holding per-account OAuth token files")
	fs.DurationVar(&rf.timeout, "timeout", 60*time.Second, "per-request HTTP timeout")
}

func (rf *resolverFlags) build() *auth.Resolver {
	return auth.NewResolver(auth.Config{
		SAKeyPath:        rf.saJSON,
		ClientSecretPath: rf.clientSecret,
		TokenDir:         rf.tokenDir,
		HTTPTimeout:      rf.timeout,
	})
}

func cmdAuth(args []string) error {
	fs := flag.NewFlagSet("auth", flag.ExitOnError)
	var rf resolverFlags
	rf.bind(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: drivemig auth <email>")
	}
	email := fs.Arg(0)
	ctx := context.Background()
	return rf.build().DeviceLogin(ctx, email)
}

func cmdPrepare(args []string) error {
	fs := flag.NewFlagSet("prepare", flag.ExitOnError)
	var rf resolverFlags
	rf.bind(fs)
	var opt manifest.Options
	logDir := fs.String("log-dir", "logs", "log directory")
	logLevel := fs.String("log-level", "info", "log level")
	fs.StringVar(&opt.PairsCSV, "csv", "", "input CSV: email_origem,email_destino (required)")
	fs.StringVar(&opt.OutFolders, "out-folders", "out_folders.csv", "output folders CSV")
	fs.StringVar(&opt.OutManifest, "out-manifest", "manifest.csv", "output manifest CSV")
	fs.StringVar(&opt.ShareFailures, "share-failures", "", "optional share-failures CSV")
	fs.StringVar(&opt.ReuseFolders, "reuse-folders", "", "optional prior folders CSV for root reuse")
	fs.StringVar(&opt.FolderMode, "folder-mode", "create_or_reuse", "create_or_reuse | must_exist")
	fs.StringVar(&opt.Role, "role", "writer", "share role: reader | commenter | writer")
	fs.BoolVar(&opt.Notify, "notify", false, "send share notification emails")
	fs.IntVar(&opt.SleepMS, "sleep-ms", 5, "sleep between API ops (ms)")
	fs.IntVar(&opt.Attempts, "max-attempts", 5, "max attempts per API call")
	fs.BoolVar(&opt.DryRun, "dry-run", false, "do not create/share/copy, just map")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if opt.PairsCSV == "" {
		return fmt.Errorf("--csv is required")
	}

	run, err := logging.Setup(*logDir, *logLevel)
	if err != nil {
		return err
	}
	defer func() { _ = run.Close() }()

	ctx := context.Background()
	return manifest.Run(ctx, run.Logger, rf.build(), opt)
}

func cmdApply(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	var rf resolverFlags
	rf.bind(fs)
	var opt apply.Options
	logDir := fs.String("log-dir", "logs", "log directory")
	logLevel := fs.String("log-level", "info", "log level")
	fs.StringVar(&opt.FoldersCSV, "folders", "", "folders CSV from prepare (required)")
	fs.StringVar(&opt.ManifestCSV, "manifest", "", "manifest CSV from prepare (required)")
	fs.StringVar(&opt.OnlyDest, "only-dest", "", "limit to one destination email")
	fs.StringVar(&opt.OnlySrc, "only-src", "", "limit to one origin email")
	fs.IntVar(&opt.Workers, "workers", 8, "concurrent copy workers")
	fs.IntVar(&opt.SleepMS, "sleep-ms", 25, "sleep between API ops per worker (ms)")
	fs.IntVar(&opt.Attempts, "max-attempts", 5, "max attempts per API call")
	fs.StringVar(&opt.UpdateChanged, "update-changed", "", `"" (full/skip), skip, replace, keep-both`)
	fs.BoolVar(&opt.DryRun, "dry-run", false, "log actions without copying")
	fs.StringVar(&opt.StateFile, "state-file", "", "JSONL state file for resume")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if opt.FoldersCSV == "" || opt.ManifestCSV == "" {
		return fmt.Errorf("--folders and --manifest are required")
	}

	run, err := logging.Setup(*logDir, *logLevel)
	if err != nil {
		return err
	}
	defer func() { _ = run.Close() }()

	ctx := context.Background()
	sum, err := apply.Run(ctx, run.Logger, rf.build(), opt)
	if err != nil {
		return err
	}
	return run.WriteSummary(sum)
}
