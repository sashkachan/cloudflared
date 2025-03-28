package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/urfave/cli/v2"
	"go.uber.org/automaxprocs/maxprocs"

	"github.com/cloudflare/cloudflared/cmd/cloudflared/access"
	"github.com/cloudflare/cloudflared/cmd/cloudflared/cliutil"
	cfdflags "github.com/cloudflare/cloudflared/cmd/cloudflared/flags"
	"github.com/cloudflare/cloudflared/cmd/cloudflared/proxydns"
	"github.com/cloudflare/cloudflared/cmd/cloudflared/tail"
	"github.com/cloudflare/cloudflared/cmd/cloudflared/tunnel"
	"github.com/cloudflare/cloudflared/cmd/cloudflared/updater"
	"github.com/cloudflare/cloudflared/config"
	"github.com/cloudflare/cloudflared/logger"
	"github.com/cloudflare/cloudflared/metrics"
	"github.com/cloudflare/cloudflared/overwatch"
	"github.com/cloudflare/cloudflared/token"
	"github.com/cloudflare/cloudflared/tracing"
	"github.com/cloudflare/cloudflared/watcher"
)

const (
	versionText = "Print the version"
)

var (
	Version   = "DEV"
	BuildTime = "unknown"
	BuildType = ""
	// Mostly network errors that we don't want reported back to Sentry, this is done by substring match.
	ignoredErrors = []string{
		"connection reset by peer",
		"An existing connection was forcibly closed by the remote host.",
		"use of closed connection",
		"You need to enable Argo Smart Routing",
		"3001 connection closed",
		"3002 connection dropped",
		"rpc exception: dial tcp",
		"rpc exception: EOF",
	}
)

func main() {
	// FIXME: TUN-8148: Disable QUIC_GO ECN due to bugs in proper detection if supported
	os.Setenv("QUIC_GO_DISABLE_ECN", "1")
	metrics.RegisterBuildInfo(BuildType, BuildTime, Version)
	_, _ = maxprocs.Set()
	bInfo := cliutil.GetBuildInfo(BuildType, Version)

	// Graceful shutdown channel used by the app. When closed, app must terminate gracefully.
	// Windows service manager closes this channel when it receives stop command.
	graceShutdownC := make(chan struct{})

	cli.VersionFlag = &cli.BoolFlag{
		Name:    "version",
		Aliases: []string{"v", "V"},
		Usage:   versionText,
	}

	app := &cli.App{}
	app.Name = "cloudflared"
	app.Usage = "Cloudflare's command-line tool and agent"
	app.UsageText = "cloudflared [global options] [command] [command options]"
	app.Copyright = fmt.Sprintf(
		`(c) %d Cloudflare Inc.
   Your installation of cloudflared software constitutes a symbol of your signature indicating that you accept
   the terms of the Apache License Version 2.0 (https://developers.cloudflare.com/cloudflare-one/connections/connect-apps/license),
   Terms (https://www.cloudflare.com/terms/) and Privacy Policy (https://www.cloudflare.com/privacypolicy/).`,
		time.Now().Year(),
	)
	app.Version = fmt.Sprintf("%s (built %s%s)", Version, BuildTime, bInfo.GetBuildTypeMsg())
	app.Description = `cloudflared connects your machine or user identity to Cloudflare's global network.
	You can use it to authenticate a session to reach an API behind Access, route web traffic to this machine,
	and configure access control.

	See https://developers.cloudflare.com/cloudflare-one/connections/connect-apps for more in-depth documentation.`
	app.Flags = flags()
	app.Action = action(graceShutdownC)
	app.Commands = commands(cli.ShowVersion)

	tunnel.Init(bInfo, graceShutdownC) // we need this to support the tunnel sub command...
	access.Init(graceShutdownC, Version)
	updater.Init(bInfo)
	tracing.Init(Version)
	token.Init(Version)
	tail.Init(bInfo)
	runApp(app, graceShutdownC)
}

func commands(version func(c *cli.Context)) []*cli.Command {
	cmds := []*cli.Command{
		{
			Name:   "update",
			Action: cliutil.ConfiguredAction(updater.Update),
			Usage:  "Update the agent if a new version exists",
			Flags: []cli.Flag{
				&cli.BoolFlag{
					Name:  "beta",
					Usage: "specify if you wish to update to the latest beta version",
				},
				&cli.BoolFlag{
					Name:   cfdflags.Force,
					Usage:  "specify if you wish to force an upgrade to the latest version regardless of the current version",
					Hidden: true,
				},
				&cli.BoolFlag{
					Name:   "staging",
					Usage:  "specify if you wish to use the staging url for updating",
					Hidden: true,
				},
				&cli.StringFlag{
					Name:   "version",
					Usage:  "specify a version you wish to upgrade or downgrade to",
					Hidden: false,
				},
			},
			Description: `Looks for a new version on the official download server.
If a new version exists, updates the agent binary and quits.
Otherwise, does nothing.

To determine if an update happened in a script, check for error code 11.`,
		},
		{
			Name: "version",
			Action: func(c *cli.Context) (err error) {
				if c.Bool("short") {
					fmt.Println(strings.Split(c.App.Version, " ")[0])
					return nil
				}
				version(c)
				return nil
			},
			Usage:       versionText,
			Description: versionText,
			Flags: []cli.Flag{
				&cli.BoolFlag{
					Name:    "short",
					Aliases: []string{"s"},
					Usage:   "print just the version number",
				},
			},
		},
	}
	cmds = append(cmds, tunnel.Commands()...)
	cmds = append(cmds, proxydns.Command(false))
	cmds = append(cmds, access.Commands()...)
	cmds = append(cmds, tail.Command())
	return cmds
}

func flags() []cli.Flag {
	flags := tunnel.Flags()
	return append(flags, access.Flags()...)
}

func isEmptyInvocation(c *cli.Context) bool {
	return c.NArg() == 0 && c.NumFlags() == 0
}

func action(graceShutdownC chan struct{}) cli.ActionFunc {
	return cliutil.ConfiguredAction(func(c *cli.Context) (err error) {
		if isEmptyInvocation(c) {
			return handleServiceMode(c, graceShutdownC)
		}
		func() {
			defer sentry.Recover()
			err = tunnel.TunnelCommand(c)
		}()
		if err != nil {
			captureError(err)
		}
		return err
	})
}

// In order to keep the amount of noise sent to Sentry low, typical network errors can be filtered out here by a substring match.
func captureError(err error) {
	errorMessage := err.Error()
	for _, ignoredErrorMessage := range ignoredErrors {
		if strings.Contains(errorMessage, ignoredErrorMessage) {
			return
		}
	}
	sentry.CaptureException(err)
}

// cloudflared was started without any flags
func handleServiceMode(c *cli.Context, shutdownC chan struct{}) error {
	log := logger.CreateLoggerFromContext(c, logger.DisableTerminalLog)

	// start the main run loop that reads from the config file
	f, err := watcher.NewFile()
	if err != nil {
		log.Err(err).Msg("Cannot load config file")
		return err
	}

	configPath := config.FindOrCreateConfigPath()
	configManager, err := config.NewFileManager(f, configPath, log)
	if err != nil {
		log.Err(err).Msg("Cannot setup config file for monitoring")
		return err
	}
	log.Info().Msgf("monitoring config file at: %s", configPath)

	serviceCallback := func(t string, name string, err error) {
		if err != nil {
			log.Err(err).Msgf("%s service: %s encountered an error", t, name)
		}
	}
	serviceManager := overwatch.NewAppManager(serviceCallback)

	appService := NewAppService(configManager, serviceManager, shutdownC, log)
	if err := appService.Run(); err != nil {
		log.Err(err).Msg("Failed to start app service")
		return err
	}
	return nil
}
