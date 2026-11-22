// e2e-control provisions Agent Kourier-owned AX fixtures and exposes cluster-local test controls.
// It uses public AX and Substrate APIs; it is never compiled into the Bedrock image.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	ate "github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	ax "github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gopkg.in/yaml.v3"
)

const space = "agent-kourier-ax"

var scenarios = []string{"default", "gated", "memory", "ask", "fails"}

type bearer struct{ path string }

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	token, err := os.ReadFile(b.path)
	if err != nil {
		return nil, err
	}
	return map[string]string{"authorization": "Bearer " + strings.TrimSpace(string(token))}, nil
}
func (bearer) RequireTransportSecurity() bool { return true }

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func run() error {
	cert, err := os.ReadFile("/run/servicedns-ca/trust-bundle.pem")
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(cert) {
		return errors.New("missing Substrate CA")
	}
	conn, err := grpc.NewClient("api.ate-system.svc.cluster.local:443", grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, ServerName: "api.ate-system.svc"})), grpc.WithPerRPCCredentials(bearer{"/var/run/secrets/ateapi/token"}))
	if err != nil {
		return err
	}
	defer conn.Close()
	control := ate.NewControlClient(conn)
	axconn, err := grpc.NewClient("ax-server.ax-system.svc.cluster.local:8080", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer axconn.Close()
	client := ax.NewAXClient(axconn)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	if err := provision(ctx, client, control); err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	target, _ := url.Parse("http://atenet-router.ate-system.svc.cluster.local")
	proxy := httputil.NewSingleHostReverseProxy(target)
	original := proxy.Director
	proxy.Director = func(r *http.Request) { original(r); r.Header.Set("ate-target-actor", space+"/gated") }
	mux.Handle("POST /_control/release/", proxy)
	mux.HandleFunc("/_control/task/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/_control/task/"), "/")
		if len(parts) != 2 || !known(parts[0]) {
			http.NotFound(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		var task *ax.Task
		var err error
		switch {
		case parts[1] == "suspend" && r.Method == http.MethodPost:
			task, err = client.SuspendTask(ctx, &ax.SuspendTaskRequest{Atespace: space, Name: parts[0]})
		case parts[1] == "pause" && r.Method == http.MethodPost:
			_, err = control.PauseActor(ctx, &ate.PauseActorRequest{Actor: &ate.ObjectRef{Atespace: space, Name: parts[0]}})
			if err == nil {
				task, err = client.GetTask(ctx, &ax.GetTaskRequest{Atespace: space, Name: parts[0]})
			}
		case parts[1] == "resume" && r.Method == http.MethodPost:
			task, err = client.ResumeTask(ctx, &ax.ResumeTaskRequest{Atespace: space, Name: parts[0]})
		case parts[1] == "status" && r.Method == http.MethodGet:
			task, err = client.GetTask(ctx, &ax.GetTaskRequest{Atespace: space, Name: parts[0]})
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		data, err := protojson.Marshal(task)
		if err != nil {
			http.Error(w, "encode task", 500)
			return
		}
		actor, err := control.GetActor(ctx, &ate.GetActorRequest{Actor: &ate.ObjectRef{Atespace: space, Name: parts[0]}})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		var result map[string]any
		if err := json.Unmarshal(data, &result); err != nil {
			http.Error(w, "encode status", 500)
			return
		}
		result["actorState"] = actor.GetStatus().GetState().String()
		data, err = json.Marshal(result)
		if err != nil {
			http.Error(w, "encode status", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	})
	return (&http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}).ListenAndServe()
}
func known(name string) bool {
	for _, s := range scenarios {
		if s == name {
			return true
		}
	}
	return false
}

func provision(ctx context.Context, client ax.AXClient, control ate.ControlClient) error {
	_, err := control.CreateAtespace(ctx, &ate.CreateAtespaceRequest{Atespace: &ate.Atespace{Metadata: &ate.ResourceMetadata{Name: space}}})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		return fmt.Errorf("create atespace: %w", err)
	}
	for _, name := range scenarios {
		existing, err := client.GetTask(ctx, &ax.GetTaskRequest{Atespace: space, Name: name})
		if err != nil && status.Code(err) != codes.NotFound {
			return err
		}
		if existing != nil && existing.GetSpec().GetImage() != os.Getenv("AX_FIXTURE_IMAGE") {
			// These named tasks live only in the dedicated, tooling-owned E2E atespace.
			// Refresh fixtures after a rebuild so tests cannot silently exercise an old binary.
			if _, err := client.DeleteTask(ctx, &ax.DeleteTaskRequest{Atespace: space, Name: name}); err != nil {
				return err
			}
			existing = nil
		}
		if existing == nil {
			task := &ax.Task{ApiVersion: "ax.io/v1alpha1", Kind: "Task", Metadata: &ax.ObjectMeta{Name: name, Atespace: space, CreationTimestamp: timestamppb.Now()}, Spec: &ax.TaskSpec{Image: os.Getenv("AX_FIXTURE_IMAGE"), Command: []string{"/usr/local/bin/ax-e2e-agent"}, Env: []*ax.EnvVar{{Name: "AX_SCENARIO", Value: name}, {Name: "OIDC_ISSUER", Value: "http://dex.dex.svc.cluster.local:5556/dex"}, {Name: "OIDC_AUDIENCE", Value: "agent-kourier-binding"}, {Name: "OIDC_SUBJECT", Value: os.Getenv("OIDC_SUBJECT")}}}}
			if task.Spec.Image == "" || os.Getenv("OIDC_SUBJECT") == "" {
				return errors.New("AX_FIXTURE_IMAGE and OIDC_SUBJECT required")
			}
			tmpl, err := template(task)
			if err != nil {
				return err
			}
			// Pre-create the template so AX reuses it: COLD_BOOT restores durable ADK data.
			if _, err = control.CreateActorTemplate(ctx, &ate.CreateActorTemplateRequest{ActorTemplate: tmpl}); err != nil && status.Code(err) != codes.AlreadyExists {
				return fmt.Errorf("template %s: %w", name, err)
			}
			if _, err = client.CreateTask(ctx, &ax.CreateTaskRequest{Task: task}); err != nil {
				return fmt.Errorf("task %s: %w", name, err)
			}
		}
		// Reconciliation creates the actor asynchronously. Add its only outbound allowance.
		actor := &ate.ObjectRef{Atespace: space, Name: name}
		for {
			_, err = control.CreateActorEgressPolicy(ctx, &ate.CreateActorEgressPolicyRequest{Actor: actor, EgressPolicy: &ate.EgressPolicy{Metadata: &ate.ResourceMetadata{Atespace: space, Name: "default"}, Rules: []*ate.EgressRule{{Hostnames: &ate.HostnameRule{Patterns: []string{"dex.dex.svc.cluster.local"}}}}}})
			if err == nil || status.Code(err) == codes.AlreadyExists {
				break
			}
			if status.Code(err) != codes.NotFound {
				return fmt.Errorf("egress %s: %w", name, err)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
		if _, err = client.ResumeTask(ctx, &ax.ResumeTaskRequest{Atespace: space, Name: name}); err != nil {
			return fmt.Errorf("resume %s: %w", name, err)
		}
	}
	return nil
}

func template(task *ax.Task) (*ate.ActorTemplate, error) {
	env := map[string]string{}
	for _, e := range task.Spec.Env {
		env[e.Name] = e.Value
	}
	data, err := yaml.Marshal(task)
	if err != nil {
		return nil, err
	}
	env["AX_TASK_YAML"] = string(data)
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	hash := sha256.New()
	_, _ = hash.Write([]byte(task.Spec.Image))
	var vars []map[string]string
	for _, k := range keys {
		_, _ = hash.Write([]byte(k + "=" + env[k] + ";"))
		vars = append(vars, map[string]string{"name": k, "value": env[k]})
	}
	// This mirrors the pinned AX template naming algorithm; no AX source is patched.
	name := fmt.Sprintf("%s-tmpl-%x", task.Metadata.Name, hash.Sum(nil)[:4])
	obj := map[string]any{
		"metadata":        map[string]string{"atespace": space, "name": name},
		"containers":      []any{map[string]any{"name": "guest", "image": task.Spec.Image, "command": []string{"/usr/local/bin/ax-task-runner"}, "env": vars, "readyz": map[string]any{"httpGet": map[string]any{"path": "/readyz", "port": 80}}, "volumeMounts": []any{map[string]string{"name": "workspace", "mountPath": "/workspace"}}}},
		"volumes":         []any{map[string]any{"name": "workspace", "durableDir": map[string]any{}}},
		"snapshotsConfig": map[string]any{"storageLocation": "gs://ate-snapshots/agent-kourier-ax/", "onPause": "SNAPSHOT_CONTENT_SCOPE_DATA", "onCommit": "SNAPSHOT_CONTENT_SCOPE_DATA", "onResume": map[string]string{"fromData": "RESUME_SOURCE_COLD_BOOT"}},
		"sandboxConfig":   map[string]string{"sandboxClass": "SANDBOX_CLASS_GVISOR", "configName": "gvisor-default"},
	}
	encoded, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var tmpl ate.ActorTemplate
	if err = protojson.Unmarshal(encoded, &tmpl); err != nil {
		return nil, err
	}
	return &tmpl, nil
}
