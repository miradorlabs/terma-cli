package cli

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// runDoctor runs doctor here, with this process's configuration and probes.
func (app *App) runDoctor(ctx context.Context, skipCommit bool, progress doctor.Progress) doctor.Report {
	return doctor.Run(ctx, app.doctorEnv(ctx, skipCommit), progress)
}

func (app *App) doctorEnv(ctx context.Context, skipCommit bool) doctor.Env {
	exe, _ := os.Executable()
	env := doctor.Env{Agents: app.agents, Exe: exe, BinDirs: app.binDirs(), SkipCommit: skipCommit}
	env.Config, env.ConfigErr = app.loadConfig()
	env.Root, env.GitDir, env.RepoErr = workspaceHere(ctx)
	if env.Config != nil {
		env.Probes = app.doctorProbes(env.Config)
	}
	return env
}

// doctorProbes reach the event spool and the platform's APIs for doctor.
func (app *App) doctorProbes(cfg *config.Config) doctor.Probes {
	return doctor.Probes{
		Spool: func() doctor.SpoolState {
			s := openSpool()
			if s == nil {
				return doctor.SpoolState{}
			}
			n, _, _ := s.Pending()
			return doctor.SpoolState{Open: true, Queued: n, WriteErr: s.Writable()}
		},
		Deliver: func(ctx context.Context) (doctor.Delivery, error) {
			res, err := app.flushSpool(ctx, true, 0)
			if err != nil {
				return doctor.Delivery{}, err
			}
			d := doctor.Delivery{Sent: res.Sent, Err: res.Err, Endpoints: res.Endpoints}
			d.Delivered, d.Undelivered = describeFlush(res)
			for _, f := range res.Failures {
				ingest, refused := errors.AsType[*spool.IngestError](f.Err)
				d.Failures = append(d.Failures, doctor.Failure{ProjectID: f.ProjectID, Endpoint: f.Endpoint, Err: f.Err,
					Refused: refused, KeyRefused: refused && ingest.KeyRefused()})
			}
			return d, nil
		},
		Endpoint:       func(projectID string) string { return app.projectEndpoint(cfg, projectID) },
		CommitRecorded: app.commitRecorded(cfg),
	}
}

// commitRecorded reads a commit's terma.commit event back where it was delivered. A
// project in another environment than the active profile's is read from its own data
// API, with its own key: the signed-in credential is bound to the active profile's auth
// host, and asking the profile's API for the project's events found nothing on every run.
func (app *App) commitRecorded(cfg *config.Config) func(ctx context.Context, projectID, sha string, from, to time.Time) (bool, error) {
	return func(ctx context.Context, projectID, sha string, from, to time.Time) (bool, error) {
		// Query the project the scratch event used, independently of command overrides.
		queryConfig := *cfg
		queryConfig.ProjectID = projectID
		if api := app.projectAPI(cfg, projectID); api != cfg.APIURL {
			if key := keystore.Get(projectID); key != "" {
				queryConfig.APIURL, queryConfig.APIKey = api, key
			}
		}
		client, err := app.newClient(&queryConfig)
		if err != nil {
			return false, err
		}
		rec, err := client.CommitLog(ctx, sha, from, to)
		return rec != nil, err
	}
}

// binaryCheck is doctor's binary check for the running build.
func (app *App) binaryCheck() doctor.Check {
	exe, _ := os.Executable()
	return doctor.BinaryCheck(exe, app.binDirs())
}
