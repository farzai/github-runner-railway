// Package reconcile decides what to create and what to remove. It is pure: the controller
// loads the facts (queued jobs, Railway services, GitHub runners) and applies the plan.
package reconcile

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"time"
)

// Scope is where a runner registers: a whole org, or one repository (a personal account has
// no org runners, so its repositories each get their own).
type Scope struct {
	Kind  string // "org" or "repo"
	Owner string
	Repo  string
}

// Key is the scope as written in configuration: org:farzai or repo:parsilver/skyabove.
func (s Scope) Key() string {
	if s.Kind == "org" {
		return "org:" + s.Owner
	}
	return "repo:" + s.Owner + "/" + s.Repo
}

// Hash is six hex characters of the key, carried in a service's name so a service can be
// matched to its scope without reading its variables.
func (s Scope) Hash() string {
	h := sha256.Sum256([]byte(s.Key()))
	return hex.EncodeToString(h[:])[:6]
}

// Service is one runner service on Railway.
type Service struct {
	ID        string
	Name      string
	ScopeHash string
	CreatedAt time.Time
	Exited    bool // the container ended (completed, failed, crashed or removed)
}

// Runner is one runner GitHub knows of in a scope.
type Runner struct {
	ID      int64
	Name    string
	Busy    bool
	Offline bool // registered but not connected: a JIT runner is offline until its container starts
}

// Params are the knobs.
type Params struct {
	MaxRunners        int
	MaxCreatePerCycle int
	StartupGrace      time.Duration // how long a service may take to register
	IdleTTL           time.Duration // how long a surplus idle runner is kept
	MaxAge            time.Duration // the job timeout plus a margin
	Scopes            []Scope
}

// Input is what Make decides from.
type Input struct {
	Now      time.Time
	Params   Params
	Queued   map[string]int // scope key -> queued jobs wanting a runner
	Services []Service
	Runners  map[string][]Runner // scope key -> registered runners
}

// Create asks for one more runner service in a scope.
type Create struct{ Scope string }

// Reap asks to remove a service, first its runner when RunnerID is set.
type Reap struct {
	Service  Service
	Scope    string
	RunnerID int64
	Force    bool // remove even if the runner is busy
	Reason   string
}

// Plan is the answer.
type Plan struct {
	Create []Create
	Reap   []Reap
}

// Make is the plan for one cycle.
//
// A service is supply for its scope while it is starting (no runner yet, or one that is still
// offline, inside the startup grace) or idle (registered, online, not busy). A busy runner is never touched, except past MaxAge,
// and a service whose container ended or that never registered is dead and goes. An idle
// runner beyond the demand goes once it is older than IdleTTL, oldest first. New services
// are created for the jobs supply does not cover, up to MaxRunners in all and
// MaxCreatePerCycle in a cycle, one scope at a time in turn so none starves.
func Make(in Input) Plan {
	var p Plan
	known := map[string]Scope{}
	for _, s := range in.Params.Scopes {
		known[s.Hash()] = s
	}
	alive := 0
	supply := map[string]int{}
	type idle struct {
		svc    Service
		runner Runner
	}
	idles := map[string][]idle{}
	for _, sv := range in.Services {
		age := in.Now.Sub(sv.CreatedAt)
		sc, ok := known[sv.ScopeHash]
		if !ok {
			// a scope taken out of the configuration may still have a job running: its services
			// go when their container ended or when the job timeout has passed
			if sv.Exited || age > in.Params.MaxAge {
				p.Reap = append(p.Reap, Reap{Service: sv, Reason: "scope no longer configured"})
			} else {
				alive++
			}
			continue
		}
		key := sc.Key()
		var runner *Runner
		for i, r := range in.Runners[key] {
			if r.Name == sv.Name {
				runner = &in.Runners[key][i]
			}
		}
		switch {
		case sv.Exited:
			p.Reap = append(p.Reap, Reap{Service: sv, Scope: key, Reason: "container ended"})
		case age > in.Params.MaxAge:
			rid := int64(0)
			if runner != nil {
				rid = runner.ID
			}
			p.Reap = append(p.Reap, Reap{Service: sv, Scope: key, RunnerID: rid, Force: true, Reason: "past the job timeout"})
		case runner == nil && age > in.Params.StartupGrace:
			p.Reap = append(p.Reap, Reap{Service: sv, Scope: key, Reason: "never registered"})
		case runner != nil && runner.Busy:
			alive++
		case runner != nil && runner.Offline && age > in.Params.StartupGrace:
			// a JIT runner is registered the moment it is made and stays offline until its
			// container connects: offline past the grace means the container is hung or gone
			p.Reap = append(p.Reap, Reap{Service: sv, Scope: key, RunnerID: runner.ID, Reason: "never came online"})
		case runner == nil || runner.Offline:
			alive++
			supply[key]++
		default:
			alive++
			supply[key]++
			idles[key] = append(idles[key], idle{sv, *runner})
		}
	}
	for _, sc := range in.Params.Scopes {
		key := sc.Key()
		surplus := supply[key] - in.Queued[key]
		list := idles[key]
		sort.Slice(list, func(i, j int) bool { return list[i].svc.CreatedAt.Before(list[j].svc.CreatedAt) })
		for _, c := range list {
			if surplus <= 0 {
				break
			}
			if in.Now.Sub(c.svc.CreatedAt) <= in.Params.IdleTTL {
				continue
			}
			p.Reap = append(p.Reap, Reap{Service: c.svc, Scope: key, RunnerID: c.runner.ID, Reason: "idle beyond demand"})
			surplus--
			alive--
		}
	}
	need := map[string]int{}
	for _, sc := range in.Params.Scopes {
		if n := in.Queued[sc.Key()] - supply[sc.Key()]; n > 0 {
			need[sc.Key()] = n
		}
	}
	room := in.Params.MaxRunners - alive
	if room > in.Params.MaxCreatePerCycle {
		room = in.Params.MaxCreatePerCycle
	}
	for room > 0 {
		made := false
		for _, sc := range in.Params.Scopes {
			if room > 0 && need[sc.Key()] > 0 {
				p.Create = append(p.Create, Create{Scope: sc.Key()})
				need[sc.Key()]--
				room--
				made = true
			}
		}
		if !made {
			break
		}
	}
	return p
}
