package api

import (
	"context"
	"net/http"
	"sync"
	"time"
)

const UsageInterval = 15 * time.Second
const usagePoints = 240

type UsagePoint struct {
	At          time.Time `json:"at"`
	CPUMilli    int64     `json:"cpuMilli"`
	MemoryBytes int64     `json:"memoryBytes"`
}

type ServerUsage struct {
	Current *UsagePoint  `json:"current"`
	History []UsagePoint `json:"history"`
}

type usageSeries struct {
	points      [usagePoints]UsagePoint
	next, count int
}

type UsageSampler struct {
	read    func(context.Context) (map[string]UsagePoint, error)
	mu      sync.RWMutex
	series  map[string]*usageSeries
	current map[string]UsagePoint
}

func NewUsageSampler(read func(context.Context) (map[string]UsagePoint, error)) *UsageSampler {
	return &UsageSampler{read: read, series: make(map[string]*usageSeries)}
}

func (u *UsageSampler) Collect(ctx context.Context, servers []ServerInfo) error {
	metrics, err := u.read(ctx)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.current = make(map[string]UsagePoint)
	if err != nil {
		return err
	}
	exists := make(map[string]bool, len(servers))
	for _, server := range servers {
		exists[server.Name] = true
		if server.Phase != "Running" || server.DesiredState != "Running" {
			continue
		}
		point, ok := metrics[server.Name+"-0"]
		if !ok {
			continue
		}
		u.current[server.Name] = point
		series := u.series[server.Name]
		if series == nil {
			series = &usageSeries{}
			u.series[server.Name] = series
		}
		if series.count > 0 && !point.At.After(series.points[(series.next+usagePoints-1)%usagePoints].At) {
			continue
		}
		series.points[series.next] = point
		series.next = (series.next + 1) % usagePoints
		series.count = min(series.count+1, usagePoints)
	}
	for name := range u.series {
		if !exists[name] {
			delete(u.series, name)
		}
	}
	return nil
}

func (u *UsageSampler) Snapshot(name string, now time.Time) ServerUsage {
	u.mu.RLock()
	defer u.mu.RUnlock()
	view := ServerUsage{History: []UsagePoint{}}
	if point, ok := u.current[name]; ok && !point.At.Before(now.Add(-3*UsageInterval)) {
		view.Current = &point
	}
	if series := u.series[name]; series != nil {
		for i := 0; i < series.count; i++ {
			point := series.points[(series.next-series.count+i+usagePoints)%usagePoints]
			if !point.At.Before(now.Add(-time.Hour)) {
				view.History = append(view.History, point)
			}
		}
	}
	return view
}

func (a *API) usageInfo(info ServerInfo) ServerInfo {
	if a.Usage != nil && info.Phase == "Running" && info.DesiredState == "Running" {
		if point := a.Usage.Snapshot(info.Name, a.now()).Current; point != nil {
			info.CPUMilli, info.MemoryBytes = &point.CPUMilli, &point.MemoryBytes
		}
	}
	return info
}

func (a *API) handleUsage(w http.ResponseWriter, r *http.Request) {
	name, ok := a.authorizeServerFiles(w, r)
	if !ok {
		return
	}
	info, err := a.Cluster.GetServer(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	view := ServerUsage{History: []UsagePoint{}}
	if a.Usage != nil {
		view = a.Usage.Snapshot(name, a.now())
	}
	if info.Phase != "Running" || info.DesiredState != "Running" {
		view.Current = nil
	}
	writeJSON(w, http.StatusOK, view)
}
