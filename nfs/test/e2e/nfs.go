/*
Copyright 2018 The Kubernetes Authors.

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

package storage

import (
	"fmt"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"io/ioutil"
	"k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
)

const (
	manifestPath       = "test/e2e/testing-manifests/"
	nfsStatefulSetName = "nfs-provisioner"

	nfsRBACCRName  = "nfs-provisioner-runner"
	nfsRBACCRBName = "run-nfs-provisioner"

	nfsClaimName = "nfs"
	nfsClaimSize = "1Mi"

	nfsClassName = "example-nfs"

	nfsWritePodName = "write-pod"

	nfsReadPodName = "read-pod"
)

var _ = Describe("external-storage", func() {
	f := framework.NewDefaultFramework("external-storage")

	// filled in BeforeEach
	var c clientset.Interface
	var ns string

	BeforeEach(func() {
		c = f.ClientSet
		ns = f.Namespace.Name
	})

	AfterEach(func() {
		c.RbacV1().ClusterRoles().Delete(nfsRBACCRName, nil)
		c.RbacV1().ClusterRoleBindings().Delete(nfsRBACCRBName, nil)
		c.StorageV1().StorageClasses().Delete(nfsClassName, nil)
	})

	Describe("NFS external provisioner", func() {
		mkpath := func(file string) string {
			return filepath.Join(framework.TestContext.RepoRoot, manifestPath, file)
		}

		It("should create and delete persistent volumes when deployed with yamls", func() {
			nsFlag := fmt.Sprintf("--namespace=%v", ns)

			By("creating nfs-provisioner RBAC")
			framework.RunKubectlOrDie("create", "-f", mkpath("rbac.yaml"))

			crb, err := c.RbacV1().ClusterRoleBindings().Get(nfsRBACCRBName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			crb.Subjects[0].Namespace = ns
			crb, err = c.RbacV1().ClusterRoleBindings().Update(crb)
			Expect(err).NotTo(HaveOccurred())

			By("creating an nfs-provisioner statefulset")
			framework.RunKubectlOrDie("create", "-f", mkpath("statefulset.yaml"), nsFlag)
			tmpDir, err := ioutil.TempDir("", "nfs-provisioner-statefulset")
			Expect(err).NotTo(HaveOccurred())

			ss, err := c.AppsV1().StatefulSets(ns).Get(nfsStatefulSetName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			sst := framework.NewStatefulSetTester(c)
			sst.WaitForRunningAndReady(*ss.Spec.Replicas, ss)

			ss, err = c.AppsV1().StatefulSets(ns).Get(nfsStatefulSetName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			ss.Spec.Template.Spec.Volumes[0].HostPath.Path = tmpDir
			ss.Spec.Template.Spec.Containers[0].Args = []string{"-grace-period=10"}
			ss, err = c.AppsV1().StatefulSets(ns).Update(ss)
			Expect(err).NotTo(HaveOccurred())

			sst.WaitForRunningAndReady(*ss.Spec.Replicas, ss)

			By("creating a class")
			framework.RunKubectlOrDie("create", "-f", mkpath("class.yaml"))

			By("checking the class")
			class, err := c.StorageV1beta1().StorageClasses().Get(nfsClassName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("creating a claim")
			framework.RunKubectlOrDie("create", "-f", mkpath("claim.yaml"), nsFlag)
			err = framework.WaitForPersistentVolumeClaimPhase(v1.ClaimBound, c, ns, nfsClaimName, framework.Poll, 60*time.Second)
			Expect(err).NotTo(HaveOccurred())

			By("checking the claim")
			// Get new copy of the claim
			claim, err := c.CoreV1().PersistentVolumeClaims(ns).Get(nfsClaimName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("checking the volume")
			// Get the bound PV
			pv, err := c.CoreV1().PersistentVolumes().Get(claim.Spec.VolumeName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			// Check sizes
			expectedCapacity := resource.MustParse(nfsClaimSize)
			pvCapacity := pv.Spec.Capacity[v1.ResourceName(v1.ResourceStorage)]
			Expect(pvCapacity.Value()).To(Equal(expectedCapacity.Value()), "pvCapacity is not equal to expectedCapacity")

			// Check PV properties
			expectedAccessModes := []v1.PersistentVolumeAccessMode{v1.ReadWriteMany}
			Expect(pv.Spec.AccessModes).To(Equal(expectedAccessModes))
			Expect(pv.Spec.ClaimRef.Name).To(Equal(claim.ObjectMeta.Name))
			Expect(pv.Spec.ClaimRef.Namespace).To(Equal(claim.ObjectMeta.Namespace))
			Expect(pv.Spec.PersistentVolumeReclaimPolicy).To(Equal(*class.ReclaimPolicy))
			Expect(pv.Spec.MountOptions).To(Equal(class.MountOptions))

			By("creating a pod to write to the volume")
			framework.RunKubectlOrDie("create", "-f", mkpath("write-pod.yaml"), nsFlag)
			framework.ExpectNoError(framework.WaitForPodSuccessInNamespace(c, nfsWritePodName, ns))

			By("creating a pod to read from the volume")
			framework.RunKubectlOrDie("create", "-f", mkpath("read-pod.yaml"), nsFlag)
			framework.ExpectNoError(framework.WaitForPodSuccessInNamespace(c, nfsReadPodName, ns))
			framework.DeletePodOrFail(c, ns, nfsReadPodName)

			By("scaling the nfs-provisioner statefulset down and up")
			sst.Restart(ss)

			By("creating a pod to read from the volume again")
			framework.RunKubectlOrDie("create", "-f", mkpath("read-pod.yaml"), nsFlag)
			framework.ExpectNoError(framework.WaitForPodSuccessInNamespace(c, nfsReadPodName, ns))

			By("deleting the claim")
			err = c.CoreV1().PersistentVolumeClaims(ns).Delete(nfsClaimName, nil)
			if err != nil && !apierrs.IsNotFound(err) {
				framework.Failf("Error deleting claim %q. Error: %v", claim.Name, err)
			}

			By("waiting for the volume to be deleted")
			if pv.Spec.PersistentVolumeReclaimPolicy == v1.PersistentVolumeReclaimDelete {
				framework.ExpectNoError(framework.WaitForPersistentVolumeDeleted(c, pv.Name, 5*time.Second, 60*time.Second))
			}
		})
	})
})
