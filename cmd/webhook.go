package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v2"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

var (
	runtimeScheme = runtime.NewScheme()
	codecs        = serializer.NewCodecFactory(runtimeScheme)
	deserializer  = codecs.UniversalDeserializer()
)

const (
	admissionWebhookAnnotationInjectKey = "env-webhook.baowj.me/inject" //
	admissionWebhookAnnotationStatusKey = "env-webhook.baowj.me/status" //
)

type WebhookServer struct {
	config *Config
	server *http.Server
}

type Config struct {
	EnvFromSources   []corev1.EnvFromSource `yaml:"envs"`
	MutateNamespaces []string               `yaml:"namespaces"`
}

type patchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

func loadConfig(configFile string) (*Config, error) {
	data, err := os.ReadFile(configFile)
	if err != nil {
		return nil, err
	}
	infoLogger.Printf("New configuration: sha256sum %x", sha256.Sum256(data))

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// Check whether the target resource need to be mutated
func mutationRequired(mutatedList []string, metadata *metav1.ObjectMeta) bool {
	annotations := metadata.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}

	required := false
	// check annotation
	if key, ok := annotations[admissionWebhookAnnotationInjectKey]; ok {
		switch strings.ToLower(key) {
		default:
			required = true
		case "n", "not", "false", "off", "disable":
			required = false
		}
	} else {
		// check namespace
		for _, namespace := range mutatedList {
			if metadata.Namespace == namespace {
				required = true
				break
			}
		}
	}

	// check status
	status := annotations[admissionWebhookAnnotationStatusKey]
	// determine whether to perform mutation based on annotation for the target resource
	if strings.ToLower(status) == "injected" {
		required = false
	}

	infoLogger.Printf("Mutation policy for %v/%v: status: %q required:%v", metadata.Namespace, metadata.Name, status, required)
	return required

}

func addEnvFrom(target []corev1.Container, added []corev1.EnvFromSource, basePath string) (patch []patchOperation) {
	for _, container := range target {
		first := container.EnvFrom == nil || len(container.EnvFrom) == 0
		var value interface{}

		for i, add := range added {
			value = add
			path := basePath + "/" + strconv.Itoa(i) + "/envFrom"
			if first {
				first = false
				value = []corev1.EnvFromSource{add}
			} else {
				path = path + "/-"
			}

			patch = append(patch, patchOperation{
				Op:    "add",
				Path:  path,
				Value: value,
			})
		}
	}
	return patch
}

func updateAnnotation(target map[string]string, added map[string]string) (patch []patchOperation) {
	if target == nil {
		target = map[string]string{}
	}

	for key, value := range added {
		if _, ok := target[key]; ok {
			patch = append(patch, patchOperation{
				Op:    "replace",
				Path:  "/metadata/annotations/" + key,
				Value: value,
			})
		} else {
			patch = append(patch, patchOperation{
				Op:   "add",
				Path: "/metadata/annotations",
				Value: map[string]string{
					key: value,
				},
			})
		}
	}
	return patch
}

// create mutation patch for resoures
func createPatch(pod *corev1.Pod, sidecarConfig *Config, annotations map[string]string) ([]byte, error) {
	var patch []patchOperation

	patch = append(patch, addEnvFrom(pod.Spec.Containers, sidecarConfig.EnvFromSources, "/spec/containers")...)
	patch = append(patch, updateAnnotation(pod.Annotations, annotations)...)

	return json.Marshal(patch)
}

// main mutation process
func (whsvr *WebhookServer) mutate(ar *admissionv1.AdmissionReview) *admissionv1.AdmissionResponse {
	req := ar.Request
	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		warningLogger.Printf("Could not unmarshal raw object: %v", err)
		return &admissionv1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}

	infoLogger.Printf("AdmissionReview for Kind=%v, Namespace=%v Name=%v (%v) UID=%v patchOperation=%v UserInfo=%v",
		req.Kind, req.Namespace, req.Name, pod.Name, req.UID, req.Operation, req.UserInfo)

	// pod.ObjectMeta.Namespace is empty if use `kubectl run xxx`
	if pod.ObjectMeta.Namespace == "" {
		pod.ObjectMeta.Namespace = req.Namespace
	}
	// determine whether to perform mutation
	if !mutationRequired(whsvr.config.MutateNamespaces, &pod.ObjectMeta) {
		infoLogger.Printf("Skipping mutation for %s/%s due to policy check", pod.Namespace, pod.Name)
		return &admissionv1.AdmissionResponse{
			Allowed: true,
		}
	}

	annotations := map[string]string{admissionWebhookAnnotationStatusKey: "injected"}
	patchBytes, err := createPatch(&pod, whsvr.config, annotations)
	if err != nil {
		return &admissionv1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}

	infoLogger.Printf("AdmissionResponse: patch=%v\n", string(patchBytes))
	return &admissionv1.AdmissionResponse{
		Allowed: true,
		Patch:   patchBytes,
		PatchType: func() *admissionv1.PatchType {
			pt := admissionv1.PatchTypeJSONPatch
			return &pt
		}(),
	}
}

// Serve method for webhook server
func (whsvr *WebhookServer) serve(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		if data, err := io.ReadAll(r.Body); err == nil {
			body = data
		}
	}
	if len(body) == 0 {
		warningLogger.Println("empty body")
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}

	// verify the content type is accurate
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		warningLogger.Printf("Content-Type=%s, expect application/json", contentType)
		http.Error(w, "invalid Content-Type, expect `application/json`", http.StatusUnsupportedMediaType)
		return
	}

	var admissionResponse *admissionv1.AdmissionResponse
	ar := admissionv1.AdmissionReview{}
	if _, _, err := deserializer.Decode(body, nil, &ar); err != nil {
		warningLogger.Printf("Can't decode body: %v", err)
		admissionResponse = &admissionv1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	} else {
		admissionResponse = whsvr.mutate(&ar)
	}

	admissionReview := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
	}
	if admissionResponse != nil {
		admissionReview.Response = admissionResponse
		if ar.Request != nil {
			admissionReview.Response.UID = ar.Request.UID
		}
	}

	resp, err := json.Marshal(admissionReview)
	if err != nil {
		warningLogger.Printf("Can't encode response: %v", err)
		http.Error(w, fmt.Sprintf("could not encode response: %v", err), http.StatusInternalServerError)
	}
	infoLogger.Printf("Ready to write reponse ...")
	if _, err := w.Write(resp); err != nil {
		warningLogger.Printf("Can't write response: %v", err)
		http.Error(w, fmt.Sprintf("could not write response: %v", err), http.StatusInternalServerError)
	}
}
