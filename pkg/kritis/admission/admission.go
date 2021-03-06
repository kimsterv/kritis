/*
Copyright 2018 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package admission

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"

	"github.com/sirupsen/logrus"

	"github.com/grafeas/kritis/pkg/kritis/admission/constants"
	kritisv1beta1 "github.com/grafeas/kritis/pkg/kritis/apis/kritis/v1beta1"
	kritisconstants "github.com/grafeas/kritis/pkg/kritis/constants"
	"github.com/grafeas/kritis/pkg/kritis/crd/securitypolicy"
	"github.com/grafeas/kritis/pkg/kritis/metadata"
	"github.com/grafeas/kritis/pkg/kritis/metadata/containeranalysis"
	"github.com/grafeas/kritis/pkg/kritis/pods"
	"github.com/grafeas/kritis/pkg/kritis/util"
	"github.com/grafeas/kritis/pkg/kritis/violation"
	"k8s.io/api/admission/v1beta1"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type config struct {
	retrievePod                 func(r *http.Request) (*v1.Pod, error)
	fetchMetadataClient         func() (metadata.MetadataFetcher, error)
	fetchImageSecurityPolicies  func(namespace string) ([]kritisv1beta1.ImageSecurityPolicy, error)
	validateImageSecurityPolicy func(isp kritisv1beta1.ImageSecurityPolicy, image string, client metadata.MetadataFetcher) ([]securitypolicy.SecurityPolicyViolation, error)
}

var (
	// For testing
	admissionConfig = config{
		retrievePod:                 unmarshalPod,
		fetchMetadataClient:         metadataClient,
		fetchImageSecurityPolicies:  securitypolicy.ImageSecurityPolicies,
		validateImageSecurityPolicy: securitypolicy.ValidateImageSecurityPolicy,
	}

	defaultViolationStrategy = violation.LoggingStrategy{}
)

// This admission controller looks for the breakglass annotation
// If one is not found, it validates against image security policies
// TODO: Check for attestations
func AdmissionReviewHandler(w http.ResponseWriter, r *http.Request) {
	logrus.Info("Starting admission review handler...")
	pod, err := admissionConfig.retrievePod(r)
	if err != nil {
		logrus.Error(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// First, check for a breakglass annotation on the pod
	if checkBreakglass(pod) {
		logrus.Debugf("found breakglass annotation, returning successful status")
		returnStatus(constants.SuccessStatus, constants.SuccessMessage, w)
		return
	}

	// TODO: Fetch Attestations for the given images to see if the image is already verified
	images := pods.Images(*pod)
	if util.CheckGlobalWhitelist(images) {
		logrus.Debugf("%s are all whitelisted, returning successful status", images)
		returnStatus(constants.SuccessStatus, constants.SuccessMessage, w)
		return
	}
	// Next, validate images in the pod against ImageSecurityPolicies in the same namespace
	isps, err := admissionConfig.fetchImageSecurityPolicies(pod.Namespace)
	if err != nil {
		logrus.Errorf("error getting image security policies: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	logrus.Debugf("Got isps %v", isps)
	// get the client we will get vulnz from
	metadataClient, err := admissionConfig.fetchMetadataClient()
	if err != nil {
		logrus.Errorf("error getting metadata client: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	for _, isp := range isps {
		for _, image := range images {
			logrus.Infof("Getting vulnz for %s", image)
			violations, err := admissionConfig.validateImageSecurityPolicy(isp, image, metadataClient)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			// Check if one of the violations is that the image is not fully qualified
			for _, v := range violations {
				if v.Violation == securitypolicy.UnqualifiedImageViolation {
					logrus.Infof("%s is not a fully qualified image", image)
					returnStatus(constants.FailureStatus, fmt.Sprintf("%s is not a fully qualified image", image), w)
					return
				}
			}
			if len(violations) != 0 {
				defaultViolationStrategy.HandleViolation(image, pod, violations)
				returnStatus(constants.FailureStatus, fmt.Sprintf("found violations in %s", image), w)
				return
			}
		}
	}
	// TODO: Create Attestations as Occurrences for the given images.
	// At this point, we can return a success status
	returnStatus(constants.SuccessStatus, constants.SuccessMessage, w)
}

func unmarshalPod(r *http.Request) (*v1.Pod, error) {
	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	ar := v1beta1.AdmissionReview{}
	if err := json.Unmarshal(data, &ar); err != nil {
		return nil, err
	}
	pod := v1.Pod{}
	if err := json.Unmarshal(ar.Request.Object.Raw, &pod); err != nil {
		return nil, err
	}
	return &pod, nil
}

func checkBreakglass(pod *v1.Pod) bool {
	annotations := pod.GetAnnotations()
	if annotations == nil {
		return false
	}
	_, ok := annotations[kritisconstants.Breakglass]
	return ok
}

// TODO: update this once we have more metadata clients
func metadataClient() (metadata.MetadataFetcher, error) {
	return containeranalysis.NewContainerAnalysisClient()
}

func returnStatus(status constants.Status, message string, w http.ResponseWriter) {
	response := &v1beta1.AdmissionResponse{
		Allowed: (status == constants.SuccessStatus),
		Result: &metav1.Status{
			Status:  string(status),
			Message: message,
		},
	}
	if err := writeHttpResponse(response, w); err != nil {
		logrus.Error("error writing response:", err)
	}
}

func writeHttpResponse(response *v1beta1.AdmissionResponse, w http.ResponseWriter) error {
	ar := v1beta1.AdmissionReview{
		Response: response,
	}
	data, err := json.Marshal(ar)
	if err != nil {
		logrus.Error(err)
		w.WriteHeader(http.StatusInternalServerError)
		return nil
	}
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(data)
	return err
}
