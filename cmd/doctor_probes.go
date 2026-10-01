package cmd

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

// wellKnownBinDirs is where doctor looks for other terma builds; tests blank it.
var wellKnownBinDirs = doctor.WellKnownBinDirs

// runDoctor runs doctor here, with this process's configuration and probes.
func runDoctor(ctx context.Context, skipCommit bool, progress doctor.Progress) doctor.Report {
	return doctor.Run(ctx, doctorEnv(ctx, skipCommit), progress)
}

func doctorEnv(ctx context.Context, skipCommit bool) doctor.Env {
	exe, _ := os.Executable()
	env := doctor.Env{Agents: registered, Exe: exe, BinDirs: wellKnownBinDirs(), SkipCommit: skipCommit}
	env.Config, env.ConfigErr = loadConfig()
	env.Root, env.GitDir, env.RepoErr = workspaceHere(ctx)
	if env.Config != nil {
		env.Probes = doctorProbes(env.Config)
	}
	return env
}

// doctorProbes reach the event spool and the platform's APIs for doctor.
func doctorProbes(cfg *config.Config) doctor.Probes {
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
			res, err := flushSpool(ctx, true, 0)
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
		Endpoint:       func(projectID string) string { return projectEndpoint(cfg, projectID) },
		CommitRecorded: commitRecorded(cfg),
	}
}

// commitRecorded reads a commit's terma.commit event back where it was delivered. A
// project in another environment than the active profile's is read from its own data
// API, with its own key: the signed-in credential is bound to the active profile's auth
// host, and asking the profile's API for the project's events found nothing on every run.
func commitRecorded(cfg *config.Config) func(ctx context.Context, projectID, sha string, from, to time.Time) (bool, error) {
	return func(ctx context.Context, projectID, sha string, from, to time.Time) (bool, error) {
		// Query the project the scratch event used, independently of command overrides.
		queryConfig := *cfg
		queryConfig.ProjectID = projectID
		if api := projectAPI(cfg, projectID); api != cfg.APIURL {
			if key := keystore.Get(projectID); key != "" {
				queryConfig.APIURL, queryConfig.APIKey = api, key
			}
		}
		client, err := newClient(&queryConfig)
		if err != nil {
			return false, err
		}
		rec, err := client.CommitLog(ctx, sha, from, to)
		return rec != nil, err
	}
}

// binaryCheck is doctor's binary check for the running build.
func binaryCheck() doctor.Check {
	exe, _ := os.Executable()
	return doctor.BinaryCheck(exe, wellKnownBinDirs())
}
