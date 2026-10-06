package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/delivery"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/relay/service"
)

func (app *App) doctorEnv(ctx context.Context) doctor.Env {
	exe, _ := os.Executable()
	env := doctor.Env{ConfigDir: app.dir, StateDir: app.stateDir, Agents: app.agents, Exe: exe, BinDirs: app.binDirs()}
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
			s := app.openSpool()
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
				ingest, refused := errors.AsType[*delivery.IngestError](f.Err)
				d.Failures = append(d.Failures, doctor.Failure{ProjectID: f.ProjectID, Endpoint: f.Endpoint, Err: f.Err,
					Refused: refused, KeyRefused: refused && ingest.KeyRefused()})
			}
			return d, nil
		},
		Relay:    app.relayFacts,
		Endpoint: func(projectID string) string { return app.delivery().Endpoint(cfg, projectID) },
		Credential: func() (doctor.Credential, error) {
			cred, err := auth.LoadCredential(app.dir, cfg.ProfileName)
			if secret.IsUnavailable(err) {
				return doctor.Credential{Locked: true}, nil
			}
			if err != nil {
				return doctor.Credential{}, err
			}
			return doctor.Credential{Email: cred.UserEmail, OrganizationID: cred.OrganizationID,
				OtherEnvironment: cred.CheckEnvironment(cfg.AuthURL) != nil, InFile: auth.StoredInFile(app.dir, cfg.ProfileName)}, nil
		},
		Keys: app.storedKeys,
	}
}

// relayFacts are the local relay's state for doctor and status.
func (app *App) relayFacts() doctor.Relay {
	dir := claim.Dir(app.stateDir)
	r := doctor.Relay{Addr: daemon.Addr(dir), Running: daemon.Running(dir)}
	r.Squatted = !r.Running && daemon.Squatted(r.Addr)
	if data, err := os.ReadFile(filepath.Join(dir, daemon.ErrorFile)); err == nil && !r.Running {
		r.LastFailure = strings.TrimSpace(string(data))
	}
	if info, ok := daemon.RunningRelay(dir); ok {
		r.Environment, r.HookStarted = info.Environment, !info.Service
	}
	if service.Supported() {
		state := daemon.CheckService(app.stateDir)
		r.ServiceInstalled, r.ServiceCurrent = state.Installed, state.Current
	}
	return r
}

// storedKeys are the delivery keys in this machine's keystore, masked.
func (app *App) storedKeys(agent, projectID string) string {
	key, err := keystore.Get(app.dir, projectID)
	if agent != "" {
		key, err = keystore.GetFor(app.dir, agent, projectID)
	}
	if err != nil {
		return "in the system keychain, which is locked or unavailable"
	}
	if key == "" {
		return ""
	}
	return keystore.Mask(key)
}
