package sensor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// ImagePull is an image a session's command had the container engine pull.
type ImagePull struct {
	Image string    `json:"image"`
	Size  int64     `json:"size"` // unpacked; the bytes pulled were the compressed layers
	Time  time.Time `json:"time"`
	Via   string    `json:"via"`
}

// containerCLIs are the commands that ask a container engine to start or pull on a
// session's behalf.
var containerCLIs = []string{"docker", "docker-compose", "podman", "nerdctl"}

// attributeWindow is how long after a session's container command the engine's start or
// pull still counts as that command's.
const attributeWindow = 30 * time.Second

// WatchDocker follows a Docker Engine's events. A container that starts, or an image that
// is pulled, within attributeWindow of a watched session's container command is that
// session's: the container's processes are counted with it from then on, though they are
// the engine's children, not the agent's.
func WatchDocker(ctx context.Context, st *State, sock string, cgroupOf func(int) (uint64, error), log *slog.Logger) {
	c := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
	for ctx.Err() == nil {
		if err := followEvents(ctx, c, st, cgroupOf, log); err != nil && ctx.Err() == nil {
			log.Warn("docker events", "err", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}

type dockerEvent struct {
	Type   string
	Action string
	Actor  struct {
		ID         string
		Attributes map[string]string
	}
	TimeNano int64 `json:"timeNano"`
}

func followEvents(ctx context.Context, c *http.Client, st *State, cgroupOf func(int) (uint64, error), log *slog.Logger) error {
	filters := url.QueryEscape(`{"type":["container","image"],"event":["start","pull"]}`)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/events?filters="+filters, nil)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var ev dockerEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		at := time.Unix(0, ev.TimeNano)
		cmd, ok := commandBefore(st, at)
		if !ok {
			continue
		}
		switch {
		case ev.Type == "container" && ev.Action == "start":
			ctr, err := inspect(ctx, c, ev.Actor.ID, cgroupOf)
			if err != nil {
				log.Warn("docker inspect", "id", ev.Actor.ID, "err", err)
				continue
			}
			ctr.Start, ctr.Via = at, strings.Join(cmd.Args, " ")
			if err := st.AddContainer(cmd.Root, ctr); err != nil {
				log.Warn("watch container", "id", ev.Actor.ID, "err", err)
			}
		case ev.Type == "image" && ev.Action == "pull":
			p := ImagePull{Image: ev.Actor.ID, Time: at, Via: strings.Join(cmd.Args, " ")}
			p.Size = imageSize(ctx, c, ev.Actor.ID)
			st.AddPull(cmd.Root, p)
		}
	}
	return sc.Err()
}

// commandBefore is the latest container command a watched session ran in the window
// before at.
func commandBefore(st *State, at time.Time) (Proc, bool) {
	execs := st.RecentExecs(at.Add(-attributeWindow))
	for i := len(execs) - 1; i >= 0; i-- {
		p := execs[i]
		if p.Start.After(at.Add(time.Second)) {
			continue
		}
		if slices.Contains(containerCLIs, filepath.Base(p.Exe)) {
			return p, true
		}
	}
	return Proc{}, false
}

func inspect(ctx context.Context, c *http.Client, id string, cgroupOf func(int) (uint64, error)) (Container, error) {
	var info struct {
		Name   string
		Config struct{ Image string }
		State  struct{ Pid int }
	}
	if err := getJSON(ctx, c, "/containers/"+id+"/json", &info); err != nil {
		return Container{}, err
	}
	ctr := Container{ID: id[:min(12, len(id))], Name: strings.TrimPrefix(info.Name, "/"), Image: info.Config.Image}
	if cgroupOf == nil { // the engine runs in a VM: its containers have no cgroup here
		return ctr, nil
	}
	cg, err := cgroupOf(info.State.Pid)
	if err != nil {
		return Container{}, fmt.Errorf("cgroup of pid %d: %w", info.State.Pid, err)
	}
	ctr.Cgroup = cg
	return ctr, nil
}

func imageSize(ctx context.Context, c *http.Client, ref string) int64 {
	var info struct{ Size int64 }
	_ = getJSON(ctx, c, "/images/"+url.PathEscape(ref)+"/json", &info)
	return info.Size
}

func getJSON(ctx context.Context, c *http.Client, path string, v any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
