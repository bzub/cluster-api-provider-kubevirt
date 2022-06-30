package controllers

import (
	gocontext "context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	kubedrain "k8s.io/kubectl/pkg/drain"
	kubevirtv1 "kubevirt.io/api/core/v1"
	infrav1 "sigs.k8s.io/cluster-api-provider-kubevirt/api/v1alpha1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/noderefutil"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type VmiReconciler struct {
	client.Client
}

func (r *VmiReconciler) SetupWithManager(ctx gocontext.Context, mgr ctrl.Manager) error {
	_, err := ctrl.NewControllerManagedBy(mgr).
		For(&kubevirtv1.VirtualMachineInstance{}).
		WithEventFilter(predicates.ResourceHasFilterLabel(ctrl.LoggerFrom(ctx), infrav1.KubevirtMachineNameLabel)).
		Build(r)

	return err
}

func (r VmiReconciler) Reconcile(ctx gocontext.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	vmi := &kubevirtv1.VirtualMachineInstance{}
	err := r.Get(ctx, req.NamespacedName, vmi)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(4).Info(fmt.Sprintf("Can't find virtualMachineInstance %s; it was already deleted.", req.NamespacedName))
			return ctrl.Result{}, nil
		}
		logger.Error(err, fmt.Sprintf("failed to read VMI %s", req.Name))
		return ctrl.Result{}, err
	}

	// KubeVirt will set the EvacuationNodeName field in case of guest node eviction. If the field is not set, there is
	// nothing to do.
	nodeName := vmi.Status.EvacuationNodeName
	if len(nodeName) == 0 { // no need to drain
		logger.V(4).Info(fmt.Sprintf("The virtualMachineInstance %s is not marked for deletion. Nothing to do here", req.NamespacedName))
		return ctrl.Result{}, nil
	}

	cluster, err := r.getCluster(ctx, vmi)
	if err != nil {
		logger.Error(err, "Can't get the cluster form the VirtualMachineInstance")
		return ctrl.Result{}, err
	}

	nodeDrained, retryDuration, err := r.drainNode(ctx, cluster, nodeName, logger)
	if err != nil || !nodeDrained {
		// logging done in the drainNode method
		return ctrl.Result{RequeueAfter: retryDuration}, err
	}

	// now, when the node is drained, we can safely delete the VMI
	propagationPolicy := metav1.DeletePropagationForeground
	err = r.Delete(ctx, vmi, &client.DeleteOptions{PropagationPolicy: &propagationPolicy})
	if err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{RequeueAfter: 20 * time.Second}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r VmiReconciler) getCluster(ctx gocontext.Context, vmi *kubevirtv1.VirtualMachineInstance) (*clusterv1.Cluster, error) {
	// get cluster from vmi
	clusterNS, ok := vmi.Labels[infrav1.KubevirtMachineNamespaceLabel]
	if !ok {
		return nil, fmt.Errorf("can't find the cluster namespace from the VM; missing %s label", infrav1.KubevirtMachineNamespaceLabel)
	}

	clusterName, ok := vmi.Labels[clusterv1.ClusterLabelName]
	if !ok {
		return nil, fmt.Errorf("can't find the cluster name from the VM; missing %s label", clusterv1.ClusterLabelName)
	}

	cluster := &clusterv1.Cluster{}
	err := r.Get(ctx, client.ObjectKey{Namespace: clusterNS, Name: clusterName}, cluster)
	if err != nil {
		return nil, fmt.Errorf("can't find the cluster %s/%s; %w", clusterNS, clusterName, err)
	}

	return cluster, nil
}

// This functions drains a node from a tenant cluster.
// The function returns 3 values:
// * drain done - boolean
// * retry time, or 0 if not needed
// * error - to be returned if we want to retry
func (r *VmiReconciler) drainNode(ctx context.Context, cluster *clusterv1.Cluster, nodeName string, logger logr.Logger) (bool, time.Duration, error) {
	kubeconfigData, err := r.getKubeconfigForWorkloadCluster(ctx, cluster)
	if err != nil {
		logger.Error(err, "Error getting a remote client configurations while deleting Machine, won't retry")
		return false, 0, nil
	}

	restConfig, err := clientcmd.RESTConfigFromKubeConfig([]byte(kubeconfigData))
	if err != nil {
		logger.Error(err, "Error generating a remote client configurations while deleting Machine, won't retry")
		return false, 0, nil
	}

	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		logger.Error(err, "Error creating a remote client while deleting Machine, won't retry")
		return false, 0, nil
	}

	node, err := kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// If an admin deletes the node directly, we'll end up here.
			logger.Error(err, "Could not find node from noderef, it may have already been deleted")
			return true, 0, nil
		}
		return false, 0, fmt.Errorf("unable to get node %q: %w", nodeName, err)
	}

	drainer := &kubedrain.Helper{
		Client:              kubeClient,
		Ctx:                 ctx,
		Force:               true,
		IgnoreAllDaemonSets: true,
		DeleteEmptyDirData:  true,
		GracePeriodSeconds:  -1,
		// If a pod is not evicted in 20 seconds, retry the eviction next time the
		// machine gets reconciled again (to allow other machines to be reconciled).
		Timeout: 20 * time.Second,
		OnPodDeletedOrEvicted: func(pod *corev1.Pod, usingEviction bool) {
			verbStr := "Deleted"
			if usingEviction {
				verbStr = "Evicted"
			}
			logger.Info(fmt.Sprintf("%s pod from Node", verbStr),
				"pod", fmt.Sprintf("%s/%s", pod.Name, pod.Namespace))
		},
		Out: writer{logger.Info},
		ErrOut: writer{func(msg string, keysAndValues ...interface{}) {
			logger.Error(nil, msg, keysAndValues...)
		}},
	}

	if noderefutil.IsNodeUnreachable(node) {
		// When the node is unreachable and some pods are not evicted for as long as this timeout, we ignore them.
		drainer.SkipWaitForDeleteTimeoutSeconds = 60 * 5 // 5 minutes
	}

	if err = kubedrain.RunCordonOrUncordon(drainer, node, true); err != nil {
		// Machine will be re-reconciled after a cordon failure.
		logger.Error(err, "Cordon failed")
		return false, 0, errors.Errorf("unable to cordon node %s: %v", nodeName, err)
	}

	if err = kubedrain.RunNodeDrain(drainer, node.Name); err != nil {
		// Machine will be re-reconciled after a drain failure.
		logger.Error(err, "Drain failed, retry in 20s", "node name", nodeName)
		return false, 20 * time.Second, nil
	}

	logger.Info("Drain successful", "node name", nodeName)
	return true, 0, nil
}

// getKubeconfigForWorkloadCluster fetches kubeconfig for workload cluster from the corresponding secret.
func (r *VmiReconciler) getKubeconfigForWorkloadCluster(ctx context.Context, cluster *clusterv1.Cluster) (string, error) {
	// workload cluster kubeconfig can be found in a secret with suffix "-kubeconfig"
	kubeconfigSecret := &corev1.Secret{}
	kubeconfigSecretKey := client.ObjectKey{Namespace: cluster.Spec.InfrastructureRef.Namespace, Name: cluster.Spec.InfrastructureRef.Name + "-kubeconfig"}
	if err := r.Client.Get(ctx, kubeconfigSecretKey, kubeconfigSecret); err != nil {
		return "", errors.Wrapf(err, "failed to fetch kubeconfig for workload cluster")
	}

	// read kubeconfig
	value, ok := kubeconfigSecret.Data["value"]
	if !ok {
		return "", errors.New("error retrieving kubeconfig data: secret value key is missing")
	}

	return string(value), nil
}

// writer implements io.Writer interface as a pass-through for klog.
type writer struct {
	logFunc func(msg string, keysAndValues ...interface{})
}

// Write passes string(p) into writer's logFunc and always returns len(p).
func (w writer) Write(p []byte) (n int, err error) {
	w.logFunc(string(p))
	return len(p), nil
}
