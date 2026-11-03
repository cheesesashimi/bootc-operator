// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	"github.com/bootc-dev/bootc-operator/internal/karpenter"
)

const (
	karpenterUpdateImage = "quay.io/zzlotnik/testing@sha256:7ce55f7717abebbe6fe6c85ad49d72a27442757c07262fcfc690d8a7abfbf357"
	karpenterNamespace   = "default"
	inflateDeployment    = "inflate-bootc"
	nonBootcDeployment   = "inflate-non-bootc"
	karpenterPool        = "karpenter"
)

// TestKarpenterBootcRollout verifies the protection and cleanup contract
// between bootc-operator and Karpenter while a Karpenter node is updated.
//
// The cluster, Karpenter, BootcNodePool, and inflate Deployment are expected
// to be provisioned by the E2E environment before this test starts.
func TestKarpenterBootcRollout(t *testing.T) {
	ctx := context.Background()
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(10 * time.Minute)
	g.SetDefaultEventuallyPollingInterval(5 * time.Second)

	c := karpenterE2EClient(t)
	t.Logf("starting Karpenter rollout test")
	if !karpenterInstalled(ctx, c) {
		t.Skip("Karpenter NodeClaim CRD is not installed")
	}

	namespace := envOrDefault("E2E_KARPENTER_NAMESPACE", karpenterNamespace)
	poolName := envOrDefault("E2E_KARPENTER_BOOTCNODEPOOL", karpenterPool)
	inflateNamespace := envOrDefault("E2E_INFLATE_NAMESPACE", namespace)

	var deployment appsv1.Deployment
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: inflateNamespace, Name: inflateDeployment}, &deployment)).
		To(Succeed())
	var pool bootcv1alpha1.BootcNodePool
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: poolName}, &pool)).
		To(Succeed())
	originalImage := pool.Spec.Image.Ref

	initialClaims := nodeClaimNames(ctx, t, c, g)
	t.Logf(
		"found %d existing NodeClaims; scaling %s/%s to 2 replicas",
		len(initialClaims),
		inflateNamespace,
		inflateDeployment,
	)
	var newClaims []*karpenter.NodeClaim
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		current := &appsv1.Deployment{}
		if err := c.Get(
			cleanupCtx,
			client.ObjectKey{Namespace: inflateNamespace, Name: inflateDeployment},
			current,
		); err == nil {
			modified := current.DeepCopy()
			zero := int32(0)
			modified.Spec.Replicas = &zero
			if err := c.Patch(cleanupCtx, modified, client.MergeFrom(current)); err != nil {
				t.Logf("scaling inflate to zero: %v", err)
			} else if err := deleteKarpenterNodes(cleanupCtx, c, newClaims); err != nil {
				t.Logf("deleting Karpenter nodes: %v", err)
			}
		}
		if err := waitForKarpenterScaleDown(
			cleanupCtx,
			t,
			c,
			inflateNamespace,
			inflateDeployment,
			newClaims,
		); err != nil {
			t.Logf("waiting for Karpenter capacity cleanup: %v", err)
		}
		currentPool := &bootcv1alpha1.BootcNodePool{}
		poolKey := client.ObjectKey{Namespace: namespace, Name: poolName}
		if err := c.Get(cleanupCtx, poolKey, currentPool); err != nil {
			t.Logf("getting BootcNodePool for image restoration: %v", err)
			return
		}
		if currentPool.Spec.Image.Ref != originalImage {
			restoredPool := currentPool.DeepCopy()
			restoredPool.Spec.Image.Ref = originalImage
			if err := c.Patch(cleanupCtx, restoredPool, client.MergeFrom(currentPool)); err != nil {
				t.Logf("restoring BootcNodePool image: %v", err)
				return
			}
		}
		g.Eventually(func() string {
			current := &bootcv1alpha1.BootcNodePool{}
			if err := c.Get(cleanupCtx, poolKey, current); err != nil {
				t.Logf("waiting for restored BootcNodePool: %v", err)
				return ""
			}
			return current.Spec.Image.Ref
		}).WithTimeout(2*time.Minute).Should(Equal(originalImage),
			"BootcNodePool %q image was not restored", poolName)
	})

	setDeploymentReplicas(ctx, t, c, &deployment, 2)
	t.Logf("waiting for Karpenter to provision a new NodeClaim")
	newClaims = waitForNewNodeClaims(ctx, t, c, g, initialClaims)
	for _, claim := range newClaims {
		waitForClaimNode(ctx, t, c, g, claim)
	}
	waitForDeploymentRunning(ctx, t, c, g, &deployment, 2)
	t.Logf(
		"initial Karpenter capacity is ready; patching BootcNodePool %q to image %q",
		pool.Name,
		karpenterUpdateImage,
	)

	modifiedPool := pool.DeepCopy()
	modifiedPool.Spec.Image.Ref = karpenterUpdateImage
	g.Expect(c.Patch(ctx, modifiedPool, client.MergeFrom(&pool))).To(Succeed())

	targetDigest := strings.SplitN(karpenterUpdateImage, "@", 2)[1]
	bootcNode, node, claim := waitForProtectedTuple(ctx, t, c, g, &pool, namespace, targetDigest)
	t.Logf(
		"found BootcNode %q, Node %q, and NodeClaim %q for the update",
		bootcNode.Name,
		node.Name,
		claim.GetName(),
	)

	t.Logf("waiting for Karpenter protection annotations on Node %q", node.Name)
	g.Eventually(func() map[string]string {
		current := &corev1.Node{}
		if err := c.Get(ctx, client.ObjectKey{Name: node.Name}, current); err != nil {
			t.Logf("waiting for Node %q repair protection; get failed: %v", node.Name, err)
			return nil
		}
		t.Logf("Node %q annotations: %v", node.Name, current.Annotations)
		return current.Annotations
	}).Should(HaveKeyWithValue(karpenter.DoNotRepairAnnotationKey, "true"))
	g.Eventually(func() map[string]string {
		current := &corev1.Node{}
		if err := c.Get(ctx, client.ObjectKey{Name: node.Name}, current); err != nil {
			t.Logf("waiting for Node %q disruption protection; get failed: %v", node.Name, err)
			return nil
		}
		return current.Annotations
	}).Should(HaveKeyWithValue(karpenter.DoNotDisruptAnnotationKey, "true"))
	g.Eventually(func() map[string]string {
		current := &bootcv1alpha1.BootcNode{}
		if err := c.Get(ctx, client.ObjectKey{Name: bootcNode.Name}, current); err != nil {
			t.Logf(
				"waiting for BootcNode %q protection annotations; get failed: %v",
				bootcNode.Name,
				err,
			)
			return nil
		}
		t.Logf("BootcNode %q annotations: %v", bootcNode.Name, current.Annotations)
		return current.Annotations
	}).Should(And(
		HaveKeyWithValue(bootcv1alpha1.AnnotationOwnsKarpenterDoNotRepair, "true"),
		HaveKeyWithValue(bootcv1alpha1.AnnotationOwnsKarpenterDoNotDisrupt, "true"),
		HaveKeyWithValue(bootcv1alpha1.AnnotationOwnsKarpenterNodeClaimDoNotRepair, "true"),
		HaveKeyWithValue(bootcv1alpha1.AnnotationOwnsKarpenterNodeClaimDoNotDisrupt, "true"),
	))
	g.Eventually(func() map[string]string {
		current := karpenter.NewNodeClaim()
		if err := c.Get(ctx, client.ObjectKey{Name: claim.GetName()}, current); err != nil {
			t.Logf(
				"waiting for NodeClaim %q protection annotations; get failed: %v",
				claim.GetName(),
				err,
			)
			return nil
		}
		t.Logf("NodeClaim %q annotations: %v", claim.GetName(), current.GetAnnotations())
		return current.GetAnnotations()
	}).Should(And(
		HaveKeyWithValue(karpenter.DoNotRepairAnnotationKey, "true"),
		HaveKeyWithValue(karpenter.DoNotDisruptAnnotationKey, "true"),
	))

	t.Logf("waiting for BootcNode %q to become Idle with digest %q", bootcNode.Name, targetDigest)
	g.Eventually(func() bool {
		current := &bootcv1alpha1.BootcNode{}
		if err := c.Get(ctx, client.ObjectKey{Name: bootcNode.Name}, current); err != nil {
			t.Logf("waiting for BootcNode update; get failed: %v", err)
			return false
		}
		t.Logf(
			"BootcNode %q status: booted=%v conditions=%v",
			bootcNode.Name,
			current.Status.Booted,
			current.Status.Conditions,
		)
		return bootcNodeIdleWithDigest(current, targetDigest)
	}).Should(BeTrue(), "BootcNode %q did not complete the update", bootcNode.Name)

	t.Logf(
		"waiting for protection annotations to be removed from Node %q, BootcNode %q, and NodeClaim %q",
		node.Name,
		bootcNode.Name,
		claim.GetName(),
	)
	g.Eventually(func() bool {
		current := &corev1.Node{}
		if err := c.Get(ctx, client.ObjectKey{Name: node.Name}, current); err != nil {
			t.Logf("waiting for Node %q protection cleanup; get failed: %v", node.Name, err)
			return false
		}
		t.Logf("Node %q annotations after update: %v", node.Name, current.Annotations)
		return annotationsAbsent(
			current.Annotations,
			karpenter.DoNotRepairAnnotationKey,
			karpenter.DoNotDisruptAnnotationKey,
		)
	}).Should(BeTrue())
	g.Eventually(func() bool {
		current := &bootcv1alpha1.BootcNode{}
		if err := c.Get(ctx, client.ObjectKey{Name: bootcNode.Name}, current); err != nil {
			t.Logf(
				"waiting for BootcNode %q protection cleanup; get failed: %v",
				bootcNode.Name,
				err,
			)
			return false
		}
		t.Logf("BootcNode %q annotations after update: %v", bootcNode.Name, current.Annotations)
		return annotationsAbsent(current.Annotations,
			bootcv1alpha1.AnnotationOwnsKarpenterDoNotRepair,
			bootcv1alpha1.AnnotationOwnsKarpenterDoNotDisrupt,
			bootcv1alpha1.AnnotationOwnsKarpenterNodeClaimDoNotRepair,
			bootcv1alpha1.AnnotationOwnsKarpenterNodeClaimDoNotDisrupt,
		)
	}).Should(BeTrue())
	g.Eventually(func() bool {
		current := karpenter.NewNodeClaim()
		if err := c.Get(ctx, client.ObjectKey{Name: claim.GetName()}, current); err != nil {
			t.Logf(
				"waiting for NodeClaim %q protection cleanup; get failed: %v",
				claim.GetName(),
				err,
			)
			return false
		}
		t.Logf(
			"NodeClaim %q annotations after update: %v",
			claim.GetName(),
			current.GetAnnotations(),
		)
		return annotationsAbsent(
			current.GetAnnotations(),
			karpenter.DoNotRepairAnnotationKey,
			karpenter.DoNotDisruptAnnotationKey,
		)
	}).Should(BeTrue())
}

// TestKarpenterBootcNodeStartupTaint verifies that a Karpenter node carries
// the bootc initialization taint until bootc-operator has verified its image.
// The watch is intentional: a later GET cannot prove that the taint was ever
// present on the newly provisioned node.
func TestKarpenterBootcNodeStartupTaint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(10 * time.Minute)
	g.SetDefaultEventuallyPollingInterval(5 * time.Second)

	c := karpenterE2EClient(t)
	kube := karpenterE2EKubernetesClient(t)
	t.Logf("starting Karpenter startup-taint test")
	if !karpenterInstalled(ctx, c) {
		t.Skip("Karpenter NodeClaim CRD is not installed")
	}

	namespace := envOrDefault("E2E_KARPENTER_NAMESPACE", karpenterNamespace)
	poolName := envOrDefault("E2E_KARPENTER_BOOTCNODEPOOL", karpenterPool)
	inflateNamespace := envOrDefault("E2E_INFLATE_NAMESPACE", namespace)

	var deployment appsv1.Deployment
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: inflateNamespace, Name: inflateDeployment}, &deployment)).
		To(Succeed())
	var pool bootcv1alpha1.BootcNodePool
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: poolName}, &pool)).
		To(Succeed())
	originalImage := pool.Spec.Image.Ref

	initialClaims := nodeClaimNames(ctx, t, c, g)
	nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	g.Expect(err).NotTo(HaveOccurred())
	t.Logf(
		"listed %d existing Nodes at resourceVersion %q",
		len(nodes.Items),
		nodes.ResourceVersion,
	)
	nodeWatch, err := kube.CoreV1().
		Nodes().
		Watch(ctx, metav1.ListOptions{ResourceVersion: nodes.ResourceVersion})
	g.Expect(err).NotTo(HaveOccurred())
	t.Logf("started Node watch from resourceVersion %q", nodes.ResourceVersion)

	var newClaims []*karpenter.NodeClaim
	t.Cleanup(func() {
		nodeWatch.Stop()
		cleanupCtx := context.Background()
		current := &appsv1.Deployment{}
		if err := c.Get(
			cleanupCtx,
			client.ObjectKey{Namespace: inflateNamespace, Name: inflateDeployment},
			current,
		); err == nil {
			modified := current.DeepCopy()
			zero := int32(0)
			modified.Spec.Replicas = &zero
			if err := c.Patch(cleanupCtx, modified, client.MergeFrom(current)); err != nil {
				t.Logf("scaling inflate to zero: %v", err)
			} else if err := deleteKarpenterNodes(cleanupCtx, c, newClaims); err != nil {
				t.Logf("deleting Karpenter nodes: %v", err)
			}
		}
		if err := waitForKarpenterScaleDown(
			cleanupCtx,
			t,
			c,
			inflateNamespace,
			inflateDeployment,
			newClaims,
		); err != nil {
			t.Logf("waiting for Karpenter capacity cleanup: %v", err)
		}
		if err := c.Get(
			cleanupCtx,
			client.ObjectKey{Namespace: namespace, Name: poolName},
			&pool,
		); err == nil {
			original := pool.DeepCopy()
			original.Spec.Image.Ref = originalImage
			if err := c.Patch(cleanupCtx, original, client.MergeFrom(&pool)); err != nil {
				t.Logf("restoring BootcNodePool image: %v", err)
			}
		}
	})

	modifiedPool := pool.DeepCopy()
	modifiedPool.Spec.Image.Ref = karpenterUpdateImage
	g.Expect(c.Patch(ctx, modifiedPool, client.MergeFrom(&pool))).To(Succeed())
	t.Logf("patched BootcNodePool %q to image %q", pool.Name, karpenterUpdateImage)

	setDeploymentReplicas(ctx, t, c, &deployment, 2)
	t.Logf(
		"scaled Deployment %s/%s to 2 replicas; waiting for a new Node taint lifecycle",
		inflateNamespace,
		deployment.Name,
	)
	lifecycle, err := waitForNewNodeTaintLifecycle(ctx, t, nodeWatch, nodes.Items)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(lifecycle.sawTaint).To(BeTrue())
	g.Expect(lifecycle.removedTaint).To(BeTrue())
	g.Expect(lifecycle.readyWhenGone).
		To(BeTrue(), "bootc.dev/os-not-ready was removed before the node became usable")

	t.Logf(
		"Node %q completed startup taint lifecycle; waiting for its NodeClaim",
		lifecycle.nodeName,
	)
	newClaims = waitForNewNodeClaims(ctx, t, c, g, initialClaims)
	var lifecycleClaim *karpenter.NodeClaim
	for _, claim := range newClaims {
		nodeName, nodeErr := claim.NodeName()
		if nodeErr == nil && nodeName == lifecycle.nodeName {
			lifecycleClaim = claim
			break
		}
	}
	g.Expect(lifecycleClaim).NotTo(BeNil(), "watched node was not created by a new NodeClaim")
	if lifecycleClaim == nil {
		t.Fatal("watched node was not created by a new NodeClaim")
	}
	waitForClaimNode(ctx, t, c, g, lifecycleClaim)
	waitForDeploymentRunning(ctx, t, c, g, &deployment, 2)
	t.Logf("startup-taint test completed successfully")
}

// TestKarpenterNonBootcNode verifies that bootc-operator ignores capacity
// provisioned for a Karpenter NodePool that is not selected by a
// BootcNodePool.
func TestKarpenterNonBootcNode(t *testing.T) {
	ctx := context.Background()
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(10 * time.Minute)
	g.SetDefaultEventuallyPollingInterval(5 * time.Second)

	c := karpenterE2EClient(t)
	t.Logf("starting non-Bootc Karpenter NodePool test")
	if !karpenterInstalled(ctx, c) {
		t.Skip("Karpenter NodeClaim CRD is not installed")
	}

	namespace := envOrDefault("E2E_KARPENTER_NAMESPACE", karpenterNamespace)
	inflateNamespace := envOrDefault("E2E_INFLATE_NAMESPACE", namespace)
	var deployment appsv1.Deployment
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: inflateNamespace, Name: nonBootcDeployment}, &deployment)).
		To(Succeed())

	initialClaims := nodeClaimNames(ctx, t, c, g)
	var newClaims []*karpenter.NodeClaim
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if err := scaleDeploymentToZero(
			cleanupCtx,
			t,
			c,
			inflateNamespace,
			nonBootcDeployment,
		); err != nil {
			t.Logf("scaling %s to zero: %v", nonBootcDeployment, err)
		} else if err := deleteKarpenterNodes(
			cleanupCtx,
			c,
			newClaims,
		); err != nil {
			t.Logf("deleting non-Bootc Karpenter nodes: %v", err)
		}
		if err := waitForKarpenterScaleDown(
			cleanupCtx,
			t,
			c,
			inflateNamespace,
			nonBootcDeployment,
			newClaims,
		); err != nil {
			t.Logf("waiting for non-Bootc Karpenter capacity cleanup: %v", err)
		}
	})

	t.Logf("scaling %s/%s to 2 replicas", inflateNamespace, nonBootcDeployment)
	setDeploymentReplicas(ctx, t, c, &deployment, 2)
	newClaims = waitForNewNodeClaims(ctx, t, c, g, initialClaims)
	for _, claim := range newClaims {
		waitForClaimNode(ctx, t, c, g, claim)
		nodeName, err := claim.NodeName()
		g.Expect(err).NotTo(HaveOccurred())
		t.Logf(
			"verifying bootc-operator ignored non-Bootc Node %q and NodeClaim %q",
			nodeName,
			claim.GetName(),
		)
		g.Eventually(func() bool {
			bootcNodes := &bootcv1alpha1.BootcNodeList{}
			if err := c.List(ctx, bootcNodes); err != nil {
				t.Logf("listing BootcNodes while checking non-Bootc Node %q: %v", nodeName, err)
				return false
			}
			for i := range bootcNodes.Items {
				if bootcNodes.Items[i].Name == nodeName {
					t.Logf("unexpected BootcNode %q exists for non-Bootc Node", nodeName)
					return false
				}
			}
			node := &corev1.Node{}
			if err := c.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
				t.Logf("checking non-Bootc Node %q: %v", nodeName, err)
				return false
			}
			claimCurrent := karpenter.NewNodeClaim()
			if err := c.Get(
				ctx,
				client.ObjectKey{Name: claim.GetName()},
				claimCurrent,
			); err != nil {
				t.Logf("checking non-Bootc NodeClaim %q: %v", claim.GetName(), err)
				return false
			}
			t.Logf(
				"non-Bootc Node %q annotations=%v taints=%v; NodeClaim annotations=%v",
				nodeName,
				node.Annotations,
				node.Spec.Taints,
				claimCurrent.GetAnnotations(),
			)
			_, managed := node.Labels[bootcv1alpha1.LabelManaged]
			return !managed &&
				annotationsAbsent(
					node.Annotations,
					karpenter.DoNotRepairAnnotationKey,
					karpenter.DoNotDisruptAnnotationKey,
				) &&
				annotationsAbsent(
					claimCurrent.GetAnnotations(),
					karpenter.DoNotRepairAnnotationKey,
					karpenter.DoNotDisruptAnnotationKey,
				) &&
				!hasOsNotReadyTaint(node)
		}).Should(BeTrue(), "bootc-operator managed non-Bootc Node %q", nodeName)
	}
	t.Logf("non-Bootc Karpenter NodePool test completed successfully")
}

type nodeTaintLifecycle struct {
	nodeName      string
	sawTaint      bool
	removedTaint  bool
	readyWhenGone bool
}

func waitForNewNodeTaintLifecycle(
	ctx context.Context,
	t *testing.T,
	nodeWatch watch.Interface,
	existing []corev1.Node,
) (nodeTaintLifecycle, error) {
	t.Helper()
	existingNames := make(map[string]struct{}, len(existing))
	for i := range existing {
		existingNames[existing[i].Name] = struct{}{}
	}
	seenTaint := make(map[string]bool)
	for {
		select {
		case <-ctx.Done():
			t.Logf("Node watch context ended: %v", ctx.Err())
			return nodeTaintLifecycle{}, ctx.Err()
		case event, ok := <-nodeWatch.ResultChan():
			if !ok {
				t.Logf("Node watch result channel closed")
				return nodeTaintLifecycle{}, fmt.Errorf("Kubernetes Node watch closed")
			}
			if event.Type == watch.Error {
				t.Logf("Node watch returned error event: %#v", event.Object)
				return nodeTaintLifecycle{}, fmt.Errorf(
					"Kubernetes Node watch failed: %v",
					event.Object,
				)
			}
			node, ok := event.Object.(*corev1.Node)
			if !ok {
				continue
			}
			if _, exists := existingNames[node.Name]; exists {
				continue
			}
			hasTaint := hasOsNotReadyTaint(node)
			t.Logf(
				"Node event=%s name=%s resourceVersion=%q tainted=%t ready=%t taints=%v",
				event.Type,
				node.Name,
				node.ResourceVersion,
				hasTaint,
				nodeReady(node),
				node.Spec.Taints,
			)
			if hasTaint {
				seenTaint[node.Name] = true
			}
			if seenTaint[node.Name] && !hasTaint {
				return nodeTaintLifecycle{
					nodeName:      node.Name,
					sawTaint:      true,
					removedTaint:  true,
					readyWhenGone: nodeReady(node),
				}, nil
			}
		}
	}
}

func hasOsNotReadyTaint(node *corev1.Node) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Key == bootcv1alpha1.TaintKeyOsNotReady &&
			taint.Effect == corev1.TaintEffectNoSchedule {
			return true
		}
	}
	return false
}

func karpenterE2EKubernetesClient(t *testing.T) kubernetes.Interface {
	t.Helper()
	path := os.Getenv("KUBECONFIG")
	if path == "" {
		t.Fatal("KUBECONFIG must be set")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		t.Fatalf("building rest config: %v", err)
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("creating Kubernetes clientset: %v", err)
	}
	return kube
}

func karpenterE2EClient(t *testing.T) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		appsv1.AddToScheme,
		apiextensionsv1.AddToScheme,
		bootcv1alpha1.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatalf("adding Kubernetes scheme: %v", err)
		}
	}
	path := os.Getenv("KUBECONFIG")
	if path == "" {
		t.Fatal("KUBECONFIG must be set")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		t.Fatalf("building rest config: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatalf("creating Kubernetes client: %v", err)
	}
	return c
}

func karpenterInstalled(ctx context.Context, c client.Client) bool {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	err := c.Get(ctx, client.ObjectKey{Name: "nodeclaims.karpenter.sh"}, crd)
	return err == nil
}

func nodeClaimNames(
	ctx context.Context,
	t *testing.T,
	c client.Client,
	g Gomega,
) map[string]struct{} {
	t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schemaGroupVersionKind())
	g.Expect(c.List(ctx, list)).To(Succeed())
	result := make(map[string]struct{}, len(list.Items))
	for _, item := range list.Items {
		result[item.GetName()] = struct{}{}
	}
	t.Logf("listed %d NodeClaims", len(result))
	return result
}

func waitForNewNodeClaims(
	ctx context.Context,
	t *testing.T,
	c client.Client,
	g Gomega,
	initial map[string]struct{},
) []*karpenter.NodeClaim {
	t.Helper()
	var result []*karpenter.NodeClaim
	g.Eventually(func() bool {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(schemaGroupVersionKind())
		if err := c.List(ctx, list); err != nil {
			t.Logf("waiting for new NodeClaim; list failed: %v", err)
			return false
		}
		result = result[:0]
		for i := range list.Items {
			if _, exists := initial[list.Items[i].GetName()]; exists {
				continue
			}
			claim, err := karpenter.NewNodeClaimFromUnstructured(&list.Items[i])
			if err == nil {
				result = append(result, claim)
			}
		}
		t.Logf("observed %d NodeClaims, %d new", len(list.Items), len(result))
		return len(result) > 0
	}).Should(BeTrue(), "Karpenter did not provision a new NodeClaim")
	return result
}

func waitForClaimNode(
	ctx context.Context,
	t *testing.T,
	c client.Client,
	g Gomega,
	claim *karpenter.NodeClaim,
) {
	t.Helper()
	g.Eventually(func() bool {
		current := karpenter.NewNodeClaim()
		if err := c.Get(ctx, client.ObjectKey{Name: claim.GetName()}, current); err != nil {
			t.Logf(
				"waiting for NodeClaim %q to reference a Ready Node; get failed: %v",
				claim.GetName(),
				err,
			)
			return false
		}
		nodeName, err := current.NodeName()
		if err != nil || nodeName == "" {
			t.Logf("NodeClaim %q has no Node yet: %v", claim.GetName(), err)
			return false
		}
		node := &corev1.Node{}
		if err := c.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
			t.Logf(
				"waiting for Node %q for NodeClaim %q; get failed: %v",
				nodeName,
				claim.GetName(),
				err,
			)
			return false
		}
		t.Logf(
			"NodeClaim %q references Node %q (ready=%t)",
			claim.GetName(),
			nodeName,
			nodeReady(node),
		)
		if !nodeReady(node) {
			return false
		}
		*claim = *current.DeepCopy()
		return true
	}).Should(BeTrue(), "NodeClaim %q did not become Ready", claim.GetName())
}

func waitForDeploymentRunning(
	ctx context.Context,
	t *testing.T,
	c client.Client,
	g Gomega,
	deployment *appsv1.Deployment,
	replicas int32,
) {
	t.Helper()
	g.Eventually(func() bool {
		current := &appsv1.Deployment{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(deployment), current); err != nil {
			t.Logf(
				"waiting for Deployment %s/%s; get failed: %v",
				deployment.Namespace,
				deployment.Name,
				err,
			)
			return false
		}
		t.Logf(
			"Deployment %s/%s status: replicas=%d ready=%d available=%d",
			deployment.Namespace,
			deployment.Name,
			current.Status.Replicas,
			current.Status.ReadyReplicas,
			current.Status.AvailableReplicas,
		)
		return current.Status.ReadyReplicas == replicas &&
			current.Status.AvailableReplicas == replicas
	}).Should(BeTrue(), "inflate did not become available with %d replicas", replicas)
}

func waitForKarpenterScaleDown(
	ctx context.Context,
	t *testing.T,
	c client.Client,
	namespace, deploymentName string,
	claims []*karpenter.NodeClaim,
) error {
	t.Helper()
	deadline := time.NewTimer(10 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		deployment := &appsv1.Deployment{}
		deploymentGoneOrScaled := false
		if err := c.Get(
			ctx,
			client.ObjectKey{Namespace: namespace, Name: deploymentName},
			deployment,
		); err == nil {
			deploymentGoneOrScaled = deployment.Status.Replicas == 0 &&
				deployment.Status.ReadyReplicas == 0
		} else if apierrors.IsNotFound(
			err,
		) {
			deploymentGoneOrScaled = true
		}

		allClaimsGone := true
		allNodesGone := true
		for _, claim := range claims {
			current := karpenter.NewNodeClaim()
			err := c.Get(ctx, client.ObjectKey{Name: claim.GetName()}, current)
			if err == nil {
				allClaimsGone = false
				nodeName, nodeErr := current.NodeName()
				if nodeErr == nil && nodeName != "" {
					node := &corev1.Node{}
					if nodeErr := c.Get(
						ctx,
						client.ObjectKey{Name: nodeName},
						node,
					); nodeErr == nil ||
						!apierrors.IsNotFound(nodeErr) {
						allNodesGone = false
					}
				}
			} else if !apierrors.IsNotFound(err) {
				allClaimsGone = false
			}
		}
		if deploymentGoneOrScaled && allClaimsGone && allNodesGone {
			t.Logf(
				"Karpenter capacity cleanup complete: deploymentGoneOrScaled=%t claimsGone=%t nodesGone=%t",
				deploymentGoneOrScaled,
				allClaimsGone,
				allNodesGone,
			)
			return nil
		}
		t.Logf(
			"waiting for Karpenter capacity cleanup: deploymentGoneOrScaled=%t claimsGone=%t nodesGone=%t",
			deploymentGoneOrScaled,
			allClaimsGone,
			allNodesGone,
		)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf(
				"Karpenter capacity did not terminate after scaling %s to zero",
				deploymentName,
			)
		case <-ticker.C:
		}
	}
}

func scaleDeploymentToZero(
	ctx context.Context,
	t *testing.T,
	c client.Client,
	namespace, deploymentName string,
) error {
	t.Helper()
	current := &appsv1.Deployment{}
	if err := c.Get(
		ctx,
		client.ObjectKey{Namespace: namespace, Name: deploymentName},
		current,
	); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting Deployment %s/%s: %w", namespace, deploymentName, err)
	}
	zero := int32(0)
	modified := current.DeepCopy()
	modified.Spec.Replicas = &zero
	if err := c.Patch(ctx, modified, client.MergeFrom(current)); err != nil {
		return fmt.Errorf("scaling Deployment %s/%s to zero: %w", namespace, deploymentName, err)
	}
	t.Logf("scaled Deployment %s/%s to zero", namespace, deploymentName)
	return nil
}

func deleteKarpenterNodes(
	ctx context.Context,
	c client.Client,
	claims []*karpenter.NodeClaim,
) error {
	for _, claim := range claims {
		current := karpenter.NewNodeClaim()
		if err := c.Get(ctx, client.ObjectKey{Name: claim.GetName()}, current); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("getting NodeClaim %q: %w", claim.GetName(), err)
		}
		nodeName, err := current.NodeName()
		if err != nil {
			continue
		}
		if nodeName == "" {
			continue
		}
		if err := c.Delete(
			ctx,
			&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}},
		); err != nil &&
			!apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting Node %q: %w", nodeName, err)
		}
	}
	return nil
}

func setDeploymentReplicas(
	ctx context.Context,
	t *testing.T,
	c client.Client,
	deployment *appsv1.Deployment,
	replicas int32,
) {
	t.Helper()
	modified := deployment.DeepCopy()
	modified.Spec.Replicas = &replicas
	if err := c.Patch(ctx, modified, client.MergeFrom(deployment)); err != nil {
		t.Fatalf("scaling inflate to %d replicas: %v", replicas, err)
	}
}

func waitForProtectedTuple(
	ctx context.Context,
	t *testing.T,
	c client.Client,
	g Gomega,
	pool *bootcv1alpha1.BootcNodePool,
	namespace, digest string,
) (*bootcv1alpha1.BootcNode, *corev1.Node, *karpenter.NodeClaim) {
	t.Helper()
	var resultBootc *bootcv1alpha1.BootcNode
	var resultNode *corev1.Node
	var resultClaim *karpenter.NodeClaim
	g.Eventually(func() bool {
		var nodes bootcv1alpha1.BootcNodeList
		if err := c.List(ctx, &nodes, client.InNamespace(namespace)); err != nil {
			t.Logf("waiting for BootcNode backed by NodeClaim; list failed: %v", err)
			return false
		}
		t.Logf("searching %d BootcNodes for an updated Karpenter-backed node", len(nodes.Items))
		for i := range nodes.Items {
			candidate := &nodes.Items[i]
			if !ownedByPool(candidate, pool) ||
				candidate.Spec.DesiredImage != karpenterUpdateImage {
				t.Logf(
					"skipping BootcNode %q: owned=%t desiredImage=%q",
					candidate.Name,
					ownedByPool(candidate, pool),
					candidate.Spec.DesiredImage,
				)
				continue
			}
			var node corev1.Node
			if err := c.Get(ctx, client.ObjectKey{Name: candidate.Name}, &node); err != nil {
				t.Logf(
					"BootcNode %q matches update but Node is not available: %v",
					candidate.Name,
					err,
				)
				continue
			}
			claims := &unstructured.UnstructuredList{}
			claims.SetGroupVersionKind(schemaGroupVersionKind())
			if err := c.List(ctx, claims); err != nil {
				t.Logf(
					"could not list NodeClaims while matching BootcNode %q: %v",
					candidate.Name,
					err,
				)
				continue
			}
			t.Logf(
				"matching BootcNode %q to Node %q across %d NodeClaims",
				candidate.Name,
				node.Name,
				len(claims.Items),
			)
			for j := range claims.Items {
				claim, err := karpenter.NewNodeClaimFromUnstructured(&claims.Items[j])
				if err != nil {
					continue
				}
				nodeName, err := claim.NodeName()
				if err == nil && nodeName == node.Name {
					resultBootc, resultNode, resultClaim = candidate.DeepCopy(), node.DeepCopy(), claim
					return true
				}
			}
		}
		return false
	}).Should(BeTrue(), "could not find a Karpenter-backed BootcNode for image digest %q", digest)
	return resultBootc, resultNode, resultClaim
}

func ownedByPool(node *bootcv1alpha1.BootcNode, pool *bootcv1alpha1.BootcNodePool) bool {
	for _, ref := range node.OwnerReferences {
		if ref.Kind == "BootcNodePool" && ref.Name == pool.Name {
			return true
		}
	}
	return false
}

func bootcNodeIdleWithDigest(node *bootcv1alpha1.BootcNode, digest string) bool {
	if node.Status.Booted == nil || node.Status.Booted.ImageDigest != digest {
		return false
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == bootcv1alpha1.NodeIdle && condition.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

func nodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func annotationsAbsent(annotations map[string]string, keys ...string) bool {
	for _, key := range keys {
		if _, found := annotations[key]; found {
			return false
		}
	}
	return true
}

func schemaGroupVersionKind() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: karpenter.Group, Version: "v1", Kind: "NodeClaimList"}
}
