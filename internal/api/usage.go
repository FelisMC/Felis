package api

import (
	"cmp"
	"context"
	"iter"
	"net/http"
	"slices"
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

// PlatformUsagePoint sums the running servers; Running counts them even when they have no metrics yet.
type PlatformUsagePoint struct {
	At          time.Time `json:"at"`
	CPUMilli    int64     `json:"cpuMilli"`
	MemoryBytes int64     `json:"memoryBytes"`
	Running     int       `json:"running"`
}

type NodeUsage struct {
	Name        string `json:"name"`
	CPUMilli    int64  `json:"cpuMilli"`
	MemoryBytes int64  `json:"memoryBytes"`
	Running     int    `json:"running"`
}

// PlatformUsage is GET /api/v1/fleet/usage; the handler fills LoginFailures24h.
type PlatformUsage struct {
	Current          *PlatformUsagePoint  `json:"current"`
	History          []PlatformUsagePoint `json:"history"`
	Nodes            []NodeUsage          `json:"nodes"`
	LoginFailures24h int                  `json:"loginFailures24h"`
}

type ring[T any] struct {
	points      [usagePoints]T
	next, count int
}

func (r *ring[T]) push(point T) {
	r.points[r.next] = point
	r.next = (r.next + 1) % usagePoints
	r.count = min(r.count+1, usagePoints)
}

func (r *ring[T]) newest() (T, bool) {
	if r.count == 0 {
		var zero T
		return zero, false
	}
	return r.points[(r.next+usagePoints-1)%usagePoints], true
}

// oldestFirst yields the stored points in chronological order.
func (r *ring[T]) oldestFirst() iter.Seq[T] {
	return func(yield func(T) bool) {
		for i := 0; i < r.count; i++ {
			if !yield(r.points[(r.next-r.count+i+usagePoints)%usagePoints]) {
				return
			}
		}
	}
}

type usageSeries = ring[UsagePoint]

type UsageSampler struct {
	read     func(context.Context) (map[string]UsagePoint, error)
	mu       sync.RWMutex
	series   map[string]*usageSeries
	current  map[string]UsagePoint
	platform ring[PlatformUsagePoint]
	nodes    []NodeUsage
}

func NewUsageSampler(read func(context.Context) (map[string]UsagePoint, error)) *UsageSampler {
	return &UsageSampler{read: read, series: make(map[string]*usageSeries)}
}

// Collect samples the metrics once; now stamps the platform point.
func (u *UsageSampler) Collect(ctx context.Context, servers []ServerInfo, now time.Time) error {
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
		if last, ok := series.newest(); ok && !point.At.After(last.At) {
			continue
		}
		series.push(point)
	}
	for name := range u.series {
		if !exists[name] {
			delete(u.series, name)
		}
	}
	platform, nodes := sumRunning(servers, metrics)
	platform.At = now.UTC()
	u.platform.push(platform)
	u.nodes = nodes
	return nil
}

// sumRunning totals the running servers, overall and per node; servers without metrics add to the counts only.
func sumRunning(servers []ServerInfo, metrics map[string]UsagePoint) (PlatformUsagePoint, []NodeUsage) {
	var total PlatformUsagePoint
	byNode := make(map[string]*NodeUsage)
	for _, server := range servers {
		if server.Phase != "Running" || server.DesiredState != "Running" {
			continue
		}
		total.Running++
		var node *NodeUsage
		if server.NodeName != "" {
			if byNode[server.NodeName] == nil {
				byNode[server.NodeName] = &NodeUsage{Name: server.NodeName}
			}
			node = byNode[server.NodeName]
			node.Running++
		}
		if point, ok := metrics[server.Name+"-0"]; ok {
			total.CPUMilli += point.CPUMilli
			total.MemoryBytes += point.MemoryBytes
			if node != nil {
				node.CPUMilli += point.CPUMilli
				node.MemoryBytes += point.MemoryBytes
			}
		}
	}
	nodes := make([]NodeUsage, 0, len(byNode))
	for _, node := range byNode {
		nodes = append(nodes, *node)
	}
	slices.SortFunc(nodes, func(a, b NodeUsage) int { return cmp.Compare(a.Name, b.Name) })
	return total, nodes
}

func (u *UsageSampler) Snapshot(name string, now time.Time) ServerUsage {
	u.mu.RLock()
	defer u.mu.RUnlock()
	view := ServerUsage{History: []UsagePoint{}}
	if point, ok := u.current[name]; ok && !point.At.Before(now.Add(-3*UsageInterval)) {
		view.Current = &point
	}
	if series := u.series[name]; series != nil {
		for point := range series.oldestFirst() {
			if !point.At.Before(now.Add(-time.Hour)) {
				view.History = append(view.History, point)
			}
		}
	}
	return view
}

// PlatformSnapshot hides the node figures with the current point once the last sample is stale.
func (u *UsageSampler) PlatformSnapshot(now time.Time) PlatformUsage {
	u.mu.RLock()
	defer u.mu.RUnlock()
	view := PlatformUsage{History: []PlatformUsagePoint{}, Nodes: []NodeUsage{}}
	if point, ok := u.platform.newest(); ok && !point.At.Before(now.Add(-3*UsageInterval)) {
		view.Current = &point
		view.Nodes = append(view.Nodes, u.nodes...)
	}
	for point := range u.platform.oldestFirst() {
		if !point.At.Before(now.Add(-time.Hour)) {
			view.History = append(view.History, point)
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
