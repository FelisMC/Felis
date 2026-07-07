package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"felis.lolicon.best/internal/naming"
	"felis.lolicon.best/internal/platform"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// backupnow is the break-glass "back up a world now" op (§B4 "Sync"). Unlike halt —
// which writes the CRD directly — a backup needs felis-api's deployment coordinates
// (FELIS_IMAGE / FELIS_BACKUP_PVC) to render the one-shot backup Job, so the console
// cannot do it in-process. It POSTs the felis-api INTERNAL face (service-token auth)
// while the API is alive, and the API renders the Job and audits the action. This file
// is the pure core (no bubbletea); tui_backupnow.go is the terminal glue.

// backupNowOutcome is the durable result of a backup request, re-printed after the TUI
// alt-screen tears down.
type backupNowOutcome struct {
	name   string
	status string // "backing_up" on success
}

// resolveInternalAPI reads the two things the on-node console needs to reach the
// felis-api internal face: the felis-api-internal Service ClusterIP (the host's
// resolver is not CoreDNS, so the cluster-DNS name is useless here) and the service
// token. Both live in the control namespace.
func resolveInternalAPI(ctx context.Context, cl client.Client, controlNamespace string) (baseURL, token string, err error) {
	var svc corev1.Service
	if err := cl.Get(ctx, types.NamespacedName{Namespace: controlNamespace, Name: platform.APIInternalServiceName}, &svc); err != nil {
		return "", "", fmt.Errorf("get %s Service: %w", platform.APIInternalServiceName, err)
	}
	ip := svc.Spec.ClusterIP
	if ip == "" || ip == corev1.ClusterIPNone {
		return "", "", fmt.Errorf("%s Service has no ClusterIP yet", platform.APIInternalServiceName)
	}

	var sec corev1.Secret
	if err := cl.Get(ctx, types.NamespacedName{Namespace: controlNamespace, Name: naming.ServiceTokenSecretName}, &sec); err != nil {
		return "", "", fmt.Errorf("get %s Secret: %w", naming.ServiceTokenSecretName, err)
	}
	token = string(sec.Data[naming.ServiceTokenSecretKey])
	if token == "" {
		return "", "", fmt.Errorf("Secret %s has no %s key", naming.ServiceTokenSecretName, naming.ServiceTokenSecretKey)
	}

	return fmt.Sprintf("http://%s:%d", ip, platform.APIInternalPort), token, nil
}

// requestBackup POSTs the internal backup endpoint and maps the response to a friendly
// outcome. osUser is sent for audit attribution (parity with halt); the API records it
// as the actor. A transport failure is distinguished from an HTTP error status because
// break-glass runs when things are broken — and this op needs the API alive by design,
// so "the API is down" is the useful message.
func requestBackup(ctx context.Context, hc *http.Client, baseURL, token, name, osUser string) (backupNowOutcome, error) {
	body, _ := json.Marshal(map[string]string{"os_user": osUser})
	url := baseURL + "/api/v1/internal/servers/" + name + "/backup"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return backupNowOutcome{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return backupNowOutcome{}, fmt.Errorf("felis-api unreachable (a backup needs it alive): %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusAccepted {
		return backupNowOutcome{name: name, status: "backing_up"}, nil
	}
	return backupNowOutcome{}, backupErrorFromResponse(resp)
}

// backupErrorFromResponse turns a non-202 into a human message. The well-known codes get
// an operator-facing explanation; anything else falls back to the API's
// {"error":{message}} body, then the bare status code.
func backupErrorFromResponse(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusConflict: // not_stopped
		return fmt.Errorf("the server must be stopped before its world can be backed up — halt it first")
	case http.StatusServiceUnavailable: // backup_unavailable
		return fmt.Errorf("the backup subsystem is not configured on felis-api (FELIS_IMAGE / FELIS_BACKUP_PVC unset)")
	case http.StatusNotFound:
		return fmt.Errorf("no such server")
	}
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	_ = json.Unmarshal(raw, &e)
	if e.Error.Message != "" {
		return fmt.Errorf("felis-api: %s", e.Error.Message)
	}
	return fmt.Errorf("felis-api returned HTTP %d", resp.StatusCode)
}

// performBackupNow composes resolve + request against a short-timeout HTTP client (a
// non-routable ClusterIP must fail fast, not hang the TUI). controlNamespace holds the
// Service + token.
func performBackupNow(ctx context.Context, cl client.Client, controlNamespace, name, osUser string) (backupNowOutcome, error) {
	baseURL, token, err := resolveInternalAPI(ctx, cl, controlNamespace)
	if err != nil {
		return backupNowOutcome{}, err
	}
	hc := &http.Client{Timeout: 10 * time.Second}
	return requestBackup(ctx, hc, baseURL, token, name, osUser)
}
