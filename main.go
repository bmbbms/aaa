package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1beta1 "k8s.io/api/networking/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type Server struct {
	clientset         *kubernetes.Clientset
	namespace         string
	host              string
	image             string
	sharedSecretName  string
	storageClassName  string
	workspaceBasePath string
	ingressClassName  string
}

type DeployRequest struct {
	Username   string `json:"username"`
	EmployeeID string `json:"employee_id"`
}

type DeployResponse struct {
	Message      string `json:"message"`
	Deployment   string `json:"deployment"`
	Service      string `json:"service"`
	Ingress      string `json:"ingress"`
	WorkspacePVC string `json:"workspace_pvc"`
	AccessURL    string `json:"access_url"`
}

func main() {
	clientset, err := buildClientset()
	if err != nil {
		log.Fatalf("build kubernetes client failed: %v", err)
	}

	s := &Server{
		clientset:         clientset,
		namespace:         env("NAMESPACE", "default"),
		host:              env("INGRESS_HOST", "copaw.example.com"),
		image:             env("COPAW_IMAGE", "agentscope/copaw:latest"),
		sharedSecretName:  env("SHARED_SECRET_NAME", "copaw-shared-secrets"),
		storageClassName:  os.Getenv("WORKSPACE_STORAGE_CLASS"),
		workspaceBasePath: env("WORKSPACE_BASE_PATH", "/app/working"),
		ingressClassName:  env("INGRESS_CLASS", "nginx"),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/copaw/deploy", s.handleDeploy)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	addr := env("LISTEN_ADDR", ":8080")
	log.Printf("copaw provisioner listening on %s", addr)
	if err := http.ListenAndServe(addr, logRequest(mux)); err != nil {
		log.Fatalf("http server failed: %v", err)
	}
}

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req DeployRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}

	if strings.TrimSpace(req.Username) == "" || strings.TrimSpace(req.EmployeeID) == "" {
		http.Error(w, "username and employee_id are required", http.StatusBadRequest)
		return
	}

	safeID, err := sanitizeToName(req.EmployeeID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	resources, err := s.ensureCopawResources(ctx, strings.TrimSpace(req.Username), safeID)
	if err != nil {
		log.Printf("deploy failed for employee %s: %v", safeID, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := DeployResponse{
		Message:      "copaw deployed successfully",
		Deployment:   resources.deployment,
		Service:      resources.service,
		Ingress:      resources.ingress,
		WorkspacePVC: resources.pvc,
		AccessURL:    fmt.Sprintf("https://%s/copaw/%s/", s.host, safeID),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

type resourceNames struct {
	deployment string
	service    string
	ingress    string
	pvc        string
}

func (s *Server) ensureCopawResources(ctx context.Context, username, employeeID string) (*resourceNames, error) {
	names := &resourceNames{
		deployment: "copaw-" + employeeID,
		service:    "copaw-" + employeeID,
		ingress:    "copaw-" + employeeID,
		pvc:        "copaw-ws-" + employeeID,
	}

	labels := map[string]string{
		"app":         "copaw",
		"employee-id": employeeID,
		"username":    username,
	}

	if err := s.applyPVC(ctx, names.pvc, labels); err != nil {
		return nil, err
	}
	if err := s.applyDeployment(ctx, names.deployment, names.pvc, labels); err != nil {
		return nil, err
	}
	if err := s.applyService(ctx, names.service, labels); err != nil {
		return nil, err
	}
	if err := s.applyIngress(ctx, names.ingress, names.service, employeeID, labels); err != nil {
		return nil, err
	}

	return names, nil
}

func (s *Server) applyPVC(ctx context.Context, name string, labels map[string]string) error {
	quantity := "10Gi"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.namespace, Labels: labels},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: mustParseQuantity(quantity)},
			},
		},
	}
	if s.storageClassName != "" {
		pvc.Spec.StorageClassName = &s.storageClassName
	}

	existing, err := s.clientset.CoreV1().PersistentVolumeClaims(s.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = s.clientset.CoreV1().PersistentVolumeClaims(s.namespace).Create(ctx, pvc, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}

	existing.Labels = merge(existing.Labels, labels)
	_, err = s.clientset.CoreV1().PersistentVolumeClaims(s.namespace).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

func (s *Server) applyDeployment(ctx context.Context, name, pvcName string, labels map[string]string) error {
	replicas := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "copaw",
						Image: s.image,
						Ports: []corev1.ContainerPort{{ContainerPort: 8088}},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "workspace", MountPath: s.workspaceBasePath},
							{Name: "shared-secret", MountPath: "/app/working.secret", ReadOnly: true},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "workspace", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName}}},
						{Name: "shared-secret", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: s.sharedSecretName}}},
					},
				},
			},
		},
	}

	existing, err := s.clientset.AppsV1().Deployments(s.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = s.clientset.AppsV1().Deployments(s.namespace).Create(ctx, dep, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}

	existing.Labels = merge(existing.Labels, labels)
	existing.Spec = dep.Spec
	_, err = s.clientset.AppsV1().Deployments(s.namespace).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

func (s *Server) applyService(ctx context.Context, name string, labels map[string]string) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.namespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       8088,
				TargetPort: intstr.FromInt(8088),
			}},
		},
	}

	existing, err := s.clientset.CoreV1().Services(s.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = s.clientset.CoreV1().Services(s.namespace).Create(ctx, svc, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}

	existing.Labels = merge(existing.Labels, labels)
	existing.Spec.Selector = svc.Spec.Selector
	existing.Spec.Ports = svc.Spec.Ports
	_, err = s.clientset.CoreV1().Services(s.namespace).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

func (s *Server) applyIngress(ctx context.Context, name, serviceName, employeeID string, labels map[string]string) error {
	ing := &networkingv1beta1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: s.namespace,
			Labels:    labels,
			Annotations: map[string]string{
				"kubernetes.io/ingress.class":                s.ingressClassName,
				"nginx.ingress.kubernetes.io/use-regex":      "true",
				"nginx.ingress.kubernetes.io/rewrite-target": "/$2",
			},
		},
		Spec: networkingv1beta1.IngressSpec{
			Rules: []networkingv1beta1.IngressRule{{
				Host: s.host,
				IngressRuleValue: networkingv1beta1.IngressRuleValue{
					HTTP: &networkingv1beta1.HTTPIngressRuleValue{Paths: []networkingv1beta1.HTTPIngressPath{{
						Path: fmt.Sprintf("/copaw/%s(/|$)(.*)", employeeID),
						Backend: networkingv1beta1.IngressBackend{
							ServiceName: serviceName,
							ServicePort: intstr.FromInt(8088),
						},
					}}},
				},
			}},
		},
	}

	existing, err := s.clientset.NetworkingV1beta1().Ingresses(s.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = s.clientset.NetworkingV1beta1().Ingresses(s.namespace).Create(ctx, ing, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}

	existing.Labels = merge(existing.Labels, labels)
	existing.Annotations = merge(existing.Annotations, ing.Annotations)
	existing.Spec = ing.Spec
	_, err = s.clientset.NetworkingV1beta1().Ingresses(s.namespace).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

func buildClientset() (*kubernetes.Clientset, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return kubernetes.NewForConfig(cfg)
	}
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = home + "/.kube/config"
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

var dnsNameRegex = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func sanitizeToName(id string) (string, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	id = strings.ReplaceAll(id, "_", "-")
	id = strings.ReplaceAll(id, ".", "-")
	if len(id) < 1 || len(id) > 63 {
		return "", errors.New("employee_id length must be between 1 and 63")
	}
	if !dnsNameRegex.MatchString(id) {
		return "", errors.New("employee_id must match kubernetes dns-1123 label after normalization")
	}
	return id, nil
}

func merge(base, add map[string]string) map[string]string {
	if base == nil {
		base = map[string]string{}
	}
	for k, v := range add {
		base[k] = v
	}
	return base
}

func env(k, fallback string) string {
	if v := os.Getenv(k); strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}

func mustParseQuantity(v string) resource.Quantity {
	q, err := resource.ParseQuantity(v)
	if err != nil {
		panic(err)
	}
	return q
}

func logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s (%s)", r.Method, r.URL.Path, time.Since(start))
	})
}
