// Copyright 2024
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/types"

	internalutils "github.com/K0rdent/kcm/internal/utils"
	"github.com/K0rdent/kcm/test/e2e/clusterdeployment"
	"github.com/K0rdent/kcm/test/e2e/clusterdeployment/aws"
	"github.com/K0rdent/kcm/test/e2e/clusterdeployment/clusteridentity"
	"github.com/K0rdent/kcm/test/e2e/config"
	"github.com/K0rdent/kcm/test/e2e/kubeclient"
	"github.com/K0rdent/kcm/test/e2e/logs"
	"github.com/K0rdent/kcm/test/e2e/templates"
	"github.com/K0rdent/kcm/test/utils"
)

type clusterInfo struct {
	// client is a kubernetes client to access the cluster
	client *kubeclient.KubeClient
	// hostedClusterName is the name of the hosted ClusterDeployment that was deployed on this cluster
	hostedClusterName string
}

var _ = Describe("AWS Templates", Label("provider:cloud", "provider:aws"), Ordered, func() {
	var (
		kc                    *kubeclient.KubeClient
		standaloneClusters    = make(map[string]clusterInfo)
		hostedDeleteFuncs     []func() error
		standaloneDeleteFuncs []func() error
		kubeconfigDeleteFuncs []func() error

		providerConfigs []config.ProviderTestingConfig
	)

	BeforeAll(func() {
		By("get testing configuration")
		providerConfigs = config.Config[config.TestingProviderAWS]

		if len(providerConfigs) == 0 {
			Skip("AWS ClusterDeployment testing is skipped")
		}

		By("providing cluster identity")
		kc = kubeclient.NewFromLocal(internalutils.DefaultSystemNamespace)
		ci := clusteridentity.New(kc, clusterdeployment.ProviderAWS)
		ci.WaitForValidCredential(kc)
		Expect(os.Setenv(clusterdeployment.EnvVarAWSClusterIdentity, ci.IdentityName)).Should(Succeed())
	})

	AfterAll(func() {
		// If we failed collect logs from each of the affiliated controllers
		// as well as the output of clusterctl to store as artifacts.
		if CurrentSpecReport().Failed() && cleanup() {
			if kc != nil {
				By("collecting failure logs from the management controllers")
				var standaloneClusterNames []string
				for clusterName := range standaloneClusters {
					standaloneClusterNames = append(standaloneClusterNames, clusterName)
				}
				logs.Collector{
					Client:        kc,
					ProviderTypes: []clusterdeployment.ProviderType{clusterdeployment.ProviderAWS, clusterdeployment.ProviderCAPI},
					ClusterNames:  standaloneClusterNames,
				}.CollectAll()
			}
			for clusterName, clusterInfo := range standaloneClusters {
				if clusterInfo.client != nil {
					By(fmt.Sprintf("collecting failure logs from controllers of the %s cluster", clusterName))
					logs.Collector{
						Client:        clusterInfo.client,
						ProviderTypes: []clusterdeployment.ProviderType{clusterdeployment.ProviderAWS, clusterdeployment.ProviderCAPI},
						ClusterNames:  []string{clusterInfo.hostedClusterName},
					}.CollectAll()
				}
			}
		}

		if cleanup() {
			By("deleting resources")
			deleteFuncs := append(hostedDeleteFuncs, append(standaloneDeleteFuncs, kubeconfigDeleteFuncs...)...)
			for _, deleteFunc := range deleteFuncs {
				err := deleteFunc()
				Expect(err).NotTo(HaveOccurred())
			}
		}
	})

	It("should work with an AWS provider", func() {
		for i, testingConfig := range providerConfigs {
			_, _ = fmt.Fprintf(GinkgoWriter, "Testing configuration:\n%s\n", testingConfig.String())
			// Deploy a standalone cluster and verify it is running/ready.
			// Deploy standalone with an xlarge instance since it will also be
			// hosting the hosted cluster.
			GinkgoT().Setenv(clusterdeployment.EnvVarAWSInstanceType, "t3.xlarge")

			sdName := clusterdeployment.GenerateClusterName(fmt.Sprintf("aws-%d", i))
			sdTemplate := testingConfig.Template
			sdTemplateType := templates.GetType(sdTemplate)

			// Supported template types for AWS standalone deployment: aws-eks, aws-standalone-cp
			Expect(sdTemplateType).To(SatisfyAny(
				Equal(templates.TemplateAWSEKS),
				Equal(templates.TemplateAWSStandaloneCP)),
				fmt.Sprintf("template type should be either %s or %s", templates.TemplateAWSEKS, templates.TemplateAWSStandaloneCP))

			templateBy(sdTemplateType, fmt.Sprintf("creating a ClusterDeployment %s with template %s", sdName, sdTemplate))

			sd := clusterdeployment.GetUnstructured(sdTemplateType, sdName, sdTemplate)

			standaloneDeleteFunc := kc.CreateClusterDeployment(context.Background(), sd)
			standaloneClusters[sdName] = clusterInfo{}
			standaloneDeleteFuncs = append(standaloneDeleteFuncs, func() error {
				By(fmt.Sprintf("Deleting the %s ClusterDeployment", sdName))
				err := standaloneDeleteFunc()
				Expect(err).NotTo(HaveOccurred())

				By(fmt.Sprintf("Verifying the %s ClusterDeployment deleted successfully", sdName))
				deletionValidator := clusterdeployment.NewProviderValidator(
					sdTemplateType,
					sdName,
					clusterdeployment.ValidationActionDelete,
				)
				Eventually(func() error {
					return deletionValidator.Validate(context.Background(), kc)
				}).WithTimeout(10 * time.Minute).WithPolling(10 *
					time.Second).Should(Succeed())
				return nil
			})

			if sdTemplateType == templates.TemplateAWSEKS {
				// TODO: w/a for https://github.com/k0rdent/kcm/issues/907. Remove when the issue is fixed.
				patch := map[string]any{
					"metadata": map[string]any{
						"annotations": map[string]string{
							"machineset.cluster.x-k8s.io/skip-preflight-checks": "ControlPlaneIsStable",
						},
					},
				}
				patchBytes, err := json.Marshal(patch)
				Expect(err).NotTo(HaveOccurred())
				Eventually(func() error {
					mds, err := kc.ListMachineDeployments(context.Background(), sdName)
					if err != nil {
						return err
					}
					if len(mds) == 0 {
						return errors.New("waiting for the MachineDeployment to be created")
					}
					_, err = kc.PatchMachineDeployment(context.Background(), mds[0].GetName(), types.MergePatchType, patchBytes)
					if err != nil {
						return err
					}
					return nil
				}, 10*time.Minute, 10*time.Second).Should(Succeed(), "Should patch MachineDeployment with \"machineset.cluster.x-k8s.io/skip-preflight-checks\": \"ControlPlaneIsStable\" annotation")
			}

			templateBy(sdTemplateType, "waiting for infrastructure to deploy successfully")
			deploymentValidator := clusterdeployment.NewProviderValidator(
				sdTemplateType,
				sdName,
				clusterdeployment.ValidationActionDeploy,
			)

			Eventually(func() error {
				return deploymentValidator.Validate(context.Background(), kc)
			}).WithTimeout(30 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())

			// validating service included in the cluster deployment is deployed
			serviceDeployedValidator := clusterdeployment.NewServiceValidator(sdName, "managed-ingress-nginx", "default").
				WithResourceValidation("service", clusterdeployment.ManagedServiceResource{
					ResourceNameSuffix: "controller",
					ValidationFunc:     clusterdeployment.ValidateService,
				}).
				WithResourceValidation("deployment", clusterdeployment.ManagedServiceResource{
					ResourceNameSuffix: "controller",
					ValidationFunc:     clusterdeployment.ValidateDeployment,
				})
			Eventually(func() error {
				return serviceDeployedValidator.Validate(context.Background(), kc)
			}).WithTimeout(10 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())

			if testingConfig.Hosted == nil {
				continue
			}

			standaloneClient := kc.NewFromCluster(context.Background(), internalutils.DefaultSystemNamespace, sdName)

			templateBy(templates.TemplateAWSHostedCP, "installing controller and templates on standalone cluster")

			// Download the KUBECONFIG for the standalone cluster and load it
			// so we can call Make targets against this cluster.
			// TODO(#472): Ideally we shouldn't use Make here and should just
			// convert these Make targets into Go code, but this will require a
			// helmclient.
			kubeCfgPath, kubecfgDeleteFunc := kc.WriteKubeconfig(context.Background(), sdName)
			kubeconfigDeleteFuncs = append(kubeconfigDeleteFuncs, kubecfgDeleteFunc)

			GinkgoT().Setenv("KUBECONFIG", kubeCfgPath)
			cmd := exec.Command("make", "test-apply")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(os.Unsetenv("KUBECONFIG")).To(Succeed())

			templateBy(templates.TemplateAWSHostedCP, "validating that the controller is ready")
			Eventually(func() error {
				err := verifyControllersUp(standaloneClient)
				if err != nil {
					_, _ = fmt.Fprintf(
						GinkgoWriter, "[%s] controller validation failed: %v\n",
						templates.TemplateAWSHostedCP, err)
					return err
				}
				return nil
			}).WithTimeout(15 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())

			// Ensure Cluster Templates in the standalone cluster are valid
			Eventually(func() error {
				err := clusterdeployment.ValidateClusterTemplates(context.Background(), standaloneClient)
				if err != nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "cluster template validation failed: %v\n", err)
					return err
				}
				return nil
			}).WithTimeout(15 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())

			// Ensure AWS credentials are set in the standalone cluster.
			standaloneCi := clusteridentity.New(standaloneClient, clusterdeployment.ProviderAWS)
			standaloneCi.WaitForValidCredential(standaloneClient)

			// Populate the environment variables required for the hosted
			// cluster.
			aws.PopulateHostedTemplateVars(context.Background(), kc, sdName)

			hdName := clusterdeployment.GenerateClusterName(fmt.Sprintf("aws-hosted-%d", i))
			hdTemplate := testingConfig.Hosted.Template
			templateBy(templates.TemplateAWSHostedCP, fmt.Sprintf("creating a hosted ClusterDeployment %s with template %s", hdName, hdTemplate))
			hd := clusterdeployment.GetUnstructured(templates.TemplateAWSHostedCP, hdName, hdTemplate)

			// Deploy the hosted cluster on top of the standalone cluster.
			hostedDeleteFunc := standaloneClient.CreateClusterDeployment(context.Background(), hd)
			hostedDeleteFuncs = append(hostedDeleteFuncs, func() error {
				By(fmt.Sprintf("Deleting the %s ClusterDeployment", hdName))
				err = hostedDeleteFunc()
				Expect(err).NotTo(HaveOccurred())

				By(fmt.Sprintf("Verifying the %s ClusterDeployment deleted successfully", hdName))
				deletionValidator := clusterdeployment.NewProviderValidator(
					templates.TemplateAWSHostedCP,
					hdName,
					clusterdeployment.ValidationActionDelete,
				)
				Eventually(func() error {
					return deletionValidator.Validate(context.Background(), standaloneClient)
				}).WithTimeout(10 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())
				return nil
			})

			standaloneClusters[sdName] = clusterInfo{
				client:            standaloneClient,
				hostedClusterName: hd.GetName(),
			}

			templateBy(templates.TemplateAWSHostedCP, "Patching AWSCluster to ready")
			clusterdeployment.PatchHostedClusterReady(standaloneClient, clusterdeployment.ProviderAWS, hdName)

			// Verify the hosted cluster is running/ready.
			templateBy(templates.TemplateAWSHostedCP, "waiting for infrastructure to deploy successfully")
			deploymentValidator = clusterdeployment.NewProviderValidator(
				templates.TemplateAWSHostedCP,
				hdName,
				clusterdeployment.ValidationActionDeploy,
			)
			Eventually(func() error {
				return deploymentValidator.Validate(context.Background(), standaloneClient)
			}).WithTimeout(30 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())
		}
	})
})
