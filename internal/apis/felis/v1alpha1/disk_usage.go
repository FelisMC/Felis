package v1alpha1

import (
	"encoding/json"
	"math"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
)

const (
	AnnotationDiskUsage         = GroupName + "/disk-usage"
	ConditionDiskSpaceAvailable = "DiskSpaceAvailable"
	DiskHealthy                 = "Healthy"
	DiskLow                     = "Low"
	StopDiskFull                = "DiskFull"
	EventDiskWarning            = "DiskWarning"
	EventDiskFull               = "DiskFull"
	DiskUsageMaxAge             = 180 * time.Second
	DefaultWorldStorageSize     = "8Gi"
)

// DiskUsageSample records a successful scan tied to the server and claim UIDs.
type DiskUsageSample struct {
	ServerUID types.UID `json:"serverUID"`
	PVCName   string    `json:"pvcName"`
	PVCUID    types.UID `json:"pvcUID"`
	UsedBytes int64     `json:"usedBytes"`
	SampledAt time.Time `json:"sampledAt"`
}

func ParseDiskUsage(raw string) (DiskUsageSample, bool) {
	var decoded struct {
		DiskUsageSample
		UsedBytes *int64 `json:"usedBytes"`
	}
	if len(raw) > 2048 || json.Unmarshal([]byte(raw), &decoded) != nil || decoded.UsedBytes == nil {
		return DiskUsageSample{}, false
	}
	sample := decoded.DiskUsageSample
	sample.UsedBytes = *decoded.UsedBytes
	if sample.ServerUID == "" || len(sample.ServerUID) > 128 ||
		sample.PVCName == "" || len(sample.PVCName) > 253 || sample.PVCUID == "" || len(sample.PVCUID) > 128 || sample.UsedBytes < 0 || sample.SampledAt.IsZero() {
		return DiskUsageSample{}, false
	}
	return sample, true
}

func ServerDiskSample(server *MinecraftServer) (DiskUsageSample, bool) {
	sample, ok := ParseDiskUsage(server.Annotations[AnnotationDiskUsage])
	return sample, ok && sample.ServerUID == server.UID && sample.PVCName == server.WorldPVC()
}

func DiskQuotaBytes(server *MinecraftServer) int64 {
	size := server.Spec.Storage.Size
	if size == "" {
		size = DefaultWorldStorageSize
	}
	q, err := resource.ParseQuantity(size)
	if err != nil || q.Sign() <= 0 || q.Cmp(*resource.NewQuantity(math.MaxInt64, resource.BinarySI)) > 0 {
		return 0
	}
	return q.Value()
}

func DiskSampleFresh(sample DiskUsageSample, now time.Time) bool {
	return !sample.SampledAt.After(now) && !sample.SampledAt.Before(now.Add(-DiskUsageMaxAge))
}

// DiskQuotaReason holds DiskFull until a fresh scan of the current claim clears it.
func DiskQuotaReason(server *MinecraftServer, pvcUID types.UID, now time.Time) string {
	sample, ok := ServerDiskSample(server)
	limit := DiskQuotaBytes(server)
	if ok && pvcUID != "" && sample.PVCUID == pvcUID && limit > 0 && !sample.SampledAt.After(now) {
		if sample.UsedBytes >= limit {
			return StopDiskFull
		}
		if DiskSampleFresh(sample, now) {
			if sample.UsedBytes >= limit-limit/10 {
				return DiskLow
			}
			return DiskHealthy
		}
	}
	if c := meta.FindStatusCondition(server.Status.Conditions, ConditionDiskSpaceAvailable); c != nil && c.Reason == StopDiskFull && c.Status == "False" {
		return StopDiskFull
	}
	return ""
}
