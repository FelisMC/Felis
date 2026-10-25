package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ServerExtraPort struct {
	Protocol string `json:"protocol"`
	Port     int32  `json:"port"`
	NodePort int32  `json:"nodePort,omitempty"`
	Address  string `json:"address,omitempty"`
}

func serverExtraPorts(server *v1alpha1.MinecraftServer) ([]ServerExtraPort, bool) {
	condition := meta.FindStatusCondition(server.Status.Conditions, v1alpha1.ConditionExtraPortsReady)
	ready := condition != nil && condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == server.Generation && len(server.Spec.ExtraPorts) == len(server.Status.ExtraPorts)
	ports := make([]ServerExtraPort, 0, len(server.Spec.ExtraPorts))
	for _, requested := range server.Spec.ExtraPorts {
		port := ServerExtraPort{Protocol: string(requested.Protocol), Port: requested.Port}
		for _, allocated := range server.Status.ExtraPorts {
			if allocated.Protocol == requested.Protocol && allocated.Port == requested.Port && allocated.NodePort > 0 {
				port.NodePort = allocated.NodePort
				break
			}
		}
		if port.NodePort == 0 {
			ready = false
		}
		ports = append(ports, port)
	}
	if !ready {
		for i := range ports {
			ports[i].NodePort = 0
		}
	}
	return ports, !ready && (len(ports) > 0 || len(server.Status.ExtraPorts) > 0 || condition != nil)
}

func (k *K8sCluster) extraPortAddresses(ctx context.Context, server *v1alpha1.MinecraftServer, info *ServerInfo) {
	if len(info.ExtraPorts) == 0 || info.ExtraPortsPending {
		return
	}
	host := k.ExtraPortsPublicIP
	if k.distributed {
		name := server.Spec.NodeName
		if server.Status.Phase == v1alpha1.PhaseRunning && server.Status.NodeName != "" {
			name = server.Status.NodeName
		}
		if name == "" {
			name = server.Status.NodeName
		}
		if name == "" {
			name = k.controller
		}
		var node corev1.Node
		if name == "" || k.c.Get(ctx, types.NamespacedName{Name: name}, &node) != nil {
			return
		}
		host = ""
		for _, address := range node.Status.Addresses {
			if address.Type == corev1.NodeExternalIP {
				host = address.Address
				break
			}
		}
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return
	}
	for i := range info.ExtraPorts {
		info.ExtraPorts[i].Address = net.JoinHostPort(ip.String(), strconv.Itoa(int(info.ExtraPorts[i].NodePort)))
	}
}

func (k *K8sCluster) ClearExtraPorts(ctx context.Context, name, uid string) error {
	err := wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, 15*time.Second, true, func(ctx context.Context) (bool, error) {
		var server v1alpha1.MinecraftServer
		if err := k.getServer(ctx, name, &server); err != nil {
			if errors.Is(err, ErrNotFound) {
				return true, nil
			}
			return false, err
		}
		if uid == "" || string(server.UID) != uid {
			return false, ErrConflict
		}
		if len(server.Spec.ExtraPorts) > 0 {
			patch := client.MergeFromWithOptions(server.DeepCopy(), client.MergeFromWithOptimisticLock{})
			server.Spec.ExtraPorts = nil
			err := k.c.Patch(ctx, &server, patch)
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return false, err
		}
		condition := meta.FindStatusCondition(server.Status.Conditions, v1alpha1.ConditionExtraPortsReady)
		return condition != nil && condition.Status == metav1.ConditionTrue && condition.ObservedGeneration >= server.Generation && len(server.Status.ExtraPorts) == 0, nil
	})
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return newError(http.StatusConflict, "extra_ports_pending", "wait for extra port cleanup to finish before releasing ownership")
	}
	return err
}
