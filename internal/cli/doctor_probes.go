package cli

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/relay/service"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

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
			return doctor.SpoolState{Open: true, Queued: n, WriteErr: s.Writable(), NextAttempt: s.NextAttempt(), Windows: s.RetryWindows(time.Now())}
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
		Relay:          relayFacts,
		Endpoint:       func(projectID string) string { return app.delivery().Endpoint(cfg, projectID) },
		CommitRecorded: app.commitRecorded(cfg),
		Credential: func() (doctor.Credential, error) {
			cred, err := auth.LoadCredential(cfg.ProfileName)
			if err != nil {
				return doctor.Credential{}, err
			}
			return doctor.Credential{Email: cred.UserEmail, OrganizationID: cred.OrganizationID,
				OtherEnvironment: cred.CheckEnvironment(cfg.AuthURL) != nil}, nil
		},
		Keys: storedKeys,
	}
}

// commitRecorded reads another environment's project from its own data API with its own
// key: the signed-in credential is bound to the active profile's auth host.
func (app *App) commitRecorded(cfg *config.Config) func(ctx context.Context, projectID, sha string, from, to time.Time) (bool, error) {
	return func(ctx context.Context, projectID, sha string, from, to time.Time) (bool, error) {
		// Query the project the scratch event used, independently of command overrides.
		queryConfig := *cfg
		queryConfig.ProjectID = projectID
		if api := app.delivery().API(cfg, projectID); api != cfg.APIURL {
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

// relayFacts are the local relay's state for doctor and status.
func relayFacts() doctor.Relay {
	dir, err := claim.Dir()
	if err != nil {
		return doctor.Relay{Err: err}
	}
	r := doctor.Relay{Dir: dir, Addr: daemon.Addr(dir), Running: daemon.Running(dir)}
	r.Squatted = !r.Running && daemon.Squatted(r.Addr)
	if info, ok := daemon.RunningRelay(dir); ok {
		r.Environment, r.HookStarted = info.Environment, !info.Service
	}
	if service.Supported() {
		state := daemon.CheckService()
		r.ServiceInstalled, r.ServiceCurrent = state.Installed, state.Current
	}
	return r
}

// storedKeys are the delivery keys in this machine's keystore, masked.
func storedKeys(agent, projectID string) string {
	key := keystore.Get(projectID)
	if agent != "" {
		key = keystore.GetFor(agent, projectID)
	}
	if key == "" {
		return ""
	}
	return keystore.Mask(key)
}
