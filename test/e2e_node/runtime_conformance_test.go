/*
Copyright 2016 The Kubernetes Authors.

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

package e2enode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/kubelet/images"
	"k8s.io/kubernetes/test/e2e/common/node"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eregistry "k8s.io/kubernetes/test/e2e/framework/registry"
	"k8s.io/kubernetes/test/e2e_node/services"
	admissionapi "k8s.io/pod-security-admission/api"

	"github.com/onsi/ginkgo/v2"
)

var _ = SIGDescribe("Container Runtime Conformance Test", func() {
	f := framework.NewDefaultFramework("runtime-conformance")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged // custom registry pods need HostPorts

	ginkgo.Describe("container runtime conformance blackbox test", func() {

		ginkgo.Context("when running a container with a new image", func() {
			// The following images are not added into NodePrePullImageList, because this test is
			// testing image pulling, these images don't need to be prepulled. The ImagePullPolicy
			// is v1.PullAlways, so it won't be blocked by framework image pre-pull list check.
			for _, testCase := range []struct {
				description  string
				image        string
				setupRegisty bool
				phase        v1.PodPhase
				waiting      bool
			}{
				{
					description:  "should be able to pull from private registry with credential provider",
					image:        "pause:testing",
					setupRegisty: true,
					phase:        v1.PodRunning,
					waiting:      false,
				},
			} {
				var registryAddress string
				var podNodes []string
				ginkgo.BeforeEach(func(ctx context.Context) {
					var err error
					if testCase.setupRegisty {
						registryAddress, podNodes, err = e2eregistry.SetupRegistry(ctx, f, true)
						framework.ExpectNoError(err)
					}
				})
				ginkgo.AfterEach(func(ctx context.Context) {
					if testCase.setupRegisty {
						f.DeleteNamespace(ctx, f.Namespace.Name) // we need to wait for the registry to be removed and so we need to delete the whole NS early (before the actual cleanup)
					}
				})

				f.It(testCase.description+"", f.WithNodeConformance(), func(ctx context.Context) {
					name := "image-pull-test"
					container := node.ConformanceContainer{
						PodClient: e2epod.NewPodClient(f),
						Container: v1.Container{
							Name:  name,
							Image: registryAddress + "/" + testCase.image,
							// PullAlways makes sure that the image will always be pulled even if it is present before the test.
							ImagePullPolicy: v1.PullAlways,
						},
						RestartPolicy: v1.RestartPolicyNever,
					}
					if testCase.setupRegisty {
						container.NodeName = podNodes[0]
					}

					auth := e2eregistry.User1DockerSecret(registryAddress).Data[v1.DockerConfigJsonKey]
					configFile := filepath.Join(services.KubeletRootDirectory, "config.json")
					err := os.WriteFile(configFile, []byte(auth), 0644)
					framework.ExpectNoError(err)
					ginkgo.DeferCleanup(func() { framework.ExpectNoError(os.Remove(configFile)) })

					// checkContainerStatus checks whether the container status matches expectation.
					checkContainerStatus := func(ctx context.Context) error {
						status, err := container.GetStatus(ctx)
						if err != nil {
							return fmt.Errorf("failed to get container status: %w", err)
						}
						// We need to check container state first. The default pod status is pending, If we check
						// pod phase first, and the expected pod phase is Pending, the container status may not
						// even show up when we check it.
						// Check container state
						if !testCase.waiting {
							if status.State.Running == nil {
								return fmt.Errorf("expected container state: Running, got: %q",
									node.GetContainerState(status.State))
							}
						}
						if testCase.waiting {
							if status.State.Waiting == nil {
								return fmt.Errorf("expected container state: Waiting, got: %q",
									node.GetContainerState(status.State))
							}
							reason := status.State.Waiting.Reason
							if reason != images.ErrImagePull.Error() &&
								reason != images.ErrImagePullBackOff.Error() {
								return fmt.Errorf("unexpected waiting reason: %q", reason)
							}
						}
						// Check pod phase
						phase, err := container.GetPhase(ctx)
						if err != nil {
							return fmt.Errorf("failed to get pod phase: %w", err)
						}
						if phase != testCase.phase {
							return fmt.Errorf("expected pod phase: %q, got: %q", testCase.phase, phase)
						}
						return nil
					}
					// The image registry is not stable, which sometimes causes the test to fail. Add retry mechanism to make this
					// less flaky.
					const flakeRetry = 3
					for i := 1; i <= flakeRetry; i++ {
						var err error
						ginkgo.By("create the container")
						container.Create(ctx)
						ginkgo.By("check the container status")
						for start := time.Now(); time.Since(start) < node.ContainerStatusRetryTimeout; time.Sleep(node.ContainerStatusPollInterval) {
							if err = checkContainerStatus(ctx); err == nil {
								break
							}
						}
						ginkgo.By("delete the container")
						_ = container.Delete(ctx)
						if err == nil {
							break
						}
						if i < flakeRetry {
							framework.Logf("No.%d attempt failed: %v, retrying...", i, err)
						} else {
							framework.Failf("All %d attempts failed: %v", flakeRetry, err)
						}
					}
				})
			}
		})
	})
})
