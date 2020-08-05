package controllers

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ipfsv1alpha1 "github.com/mfarghaly/kotal/apis/ipfs/v1alpha1"
)

// SwarmReconciler reconciles a Swarm object
type SwarmReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=ipfs.kotal.io,resources=swarms,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ipfs.kotal.io,resources=swarms/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=watch;get;list;create;update;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=watch;get;create;update;list;delete

// Reconcile reconciles ipfs swarm
func (r *SwarmReconciler) Reconcile(req ctrl.Request) (result ctrl.Result, err error) {
	ctx := context.Background()
	_ = r.Log.WithValues("swarm", req.NamespacedName)

	var swarm ipfsv1alpha1.Swarm

	if err = r.Client.Get(ctx, req.NamespacedName, &swarm); err != nil {
		err = client.IgnoreNotFound(err)
		return
	}

	if err = r.reconcileNodes(ctx, &swarm); err != nil {
		return
	}

	return
}

// reconcileNodes reconcile ipfs swarm nodes
func (r *SwarmReconciler) reconcileNodes(ctx context.Context, swarm *ipfsv1alpha1.Swarm) error {
	peers := []string{}
	for _, node := range swarm.Spec.Nodes {
		addr, err := r.reconcileNode(ctx, &node, swarm, peers)
		if err != nil {
			return err
		}
		peers = append(peers, addr)
	}
	return nil
}

// reconcileNode reconciles a single ipfs node
// it creates node deployment, service and data pvc if it doesn't exist
func (r *SwarmReconciler) reconcileNode(ctx context.Context, node *ipfsv1alpha1.Node, swarm *ipfsv1alpha1.Swarm, peers []string) (addr string, err error) {
	var ip string

	if ip, err = r.reconcileNodeService(ctx, node, swarm); err != nil {
		return
	}

	if err = r.reconcileNodeDeployment(ctx, node, swarm, peers); err != nil {
		return
	}

	addr = node.SwarmAddress(ip)

	return
}

// reconcileNodeService reconciles node service
func (r *SwarmReconciler) reconcileNodeService(ctx context.Context, node *ipfsv1alpha1.Node, swarm *ipfsv1alpha1.Swarm) (string, error) {

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      node.Name,
			Namespace: swarm.Namespace,
		},
	}

	_, err := ctrl.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if err := ctrl.SetControllerReference(swarm, svc, r.Scheme); err != nil {
			return err
		}
		r.specNodeService(svc, node)
		return nil
	})

	return svc.Spec.ClusterIP, err
}

// specNodeService updates node service spec
func (r *SwarmReconciler) specNodeService(svc *corev1.Service, node *ipfsv1alpha1.Node) {

	labels := map[string]string{
		"name":     "node",
		"instance": node.Name,
	}
	svc.ObjectMeta.Labels = labels

	svc.Spec.Ports = []corev1.ServicePort{
		{
			Name:       "swarm",
			Port:       4001,
			TargetPort: intstr.FromInt(4001),
			Protocol:   corev1.ProtocolTCP,
		},
		{
			Name:       "swarm-udp",
			Port:       4002,
			TargetPort: intstr.FromInt(4002),
			Protocol:   corev1.ProtocolUDP,
		},
		{
			Name:       "api",
			Port:       5001,
			TargetPort: intstr.FromInt(5001),
			Protocol:   corev1.ProtocolUDP,
		},
		{
			Name:       "gateway",
			Port:       8080,
			TargetPort: intstr.FromInt(8080),
			Protocol:   corev1.ProtocolUDP,
		},
	}

	svc.Spec.Selector = labels

}

// reconcileNodeDeployment reconciles node deployment
func (r *SwarmReconciler) reconcileNodeDeployment(ctx context.Context, node *ipfsv1alpha1.Node, swarm *ipfsv1alpha1.Swarm, peers []string) error {

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      node.Name,
			Namespace: swarm.Namespace,
		},
	}

	_, err := ctrl.CreateOrUpdate(ctx, r.Client, dep, func() error {
		if err := ctrl.SetControllerReference(swarm, dep, r.Scheme); err != nil {
			return err
		}
		r.specNodeDeployment(dep, node, peers)
		return nil
	})

	return err
}

// specNodeDeployment updates node deployment spec
func (r *SwarmReconciler) specNodeDeployment(dep *appsv1.Deployment, node *ipfsv1alpha1.Node, peers []string) {

	dep.ObjectMeta.Labels = map[string]string{
		"name":     "node",
		"instance": node.Name,
	}

	initContainers := []corev1.Container{}

	initNode := corev1.Container{
		Name:  "init-node",
		Image: "kotalco/go-ipfs:v0.6.0",
		Env: []corev1.EnvVar{
			{
				Name:  "IPFS_PEER_ID",
				Value: node.ID,
			},
			{
				Name:  "IPFS_PRIVATE_KEY",
				Value: node.PrivateKey,
			},
		},
		Command: []string{"ipfs"},
		Args:    []string{"init"},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "data",
				MountPath: "/data/ipfs",
			},
		},
	}
	initContainers = append(initContainers, initNode)

	for i, peer := range peers {
		addBootstrapPeer := corev1.Container{
			Name:    fmt.Sprintf("add-bootstrap-peer-%d", i),
			Image:   "ipfs/go-ipfs:v0.6.0",
			Command: []string{"ipfs"},
			Args:    []string{"bootstrap", "add", peer},
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      "data",
					MountPath: "/data/ipfs",
				},
			},
		}
		initContainers = append(initContainers, addBootstrapPeer)
	}

	for _, profile := range node.Profiles {
		applyProfile := corev1.Container{
			Name:    fmt.Sprintf("apply-%s-profile", profile),
			Image:   "ipfs/go-ipfs:v0.6.0",
			Command: []string{"ipfs"},
			Args:    []string{"config", "profile", "apply", string(profile)},
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      "data",
					MountPath: "/data/ipfs",
				},
			},
		}
		initContainers = append(initContainers, applyProfile)
	}

	dep.Spec = appsv1.DeploymentSpec{
		Selector: &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"name":     "node",
				"instance": node.Name,
			},
		},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					"name":     "node",
					"instance": node.Name,
				},
			},
			Spec: corev1.PodSpec{
				InitContainers: initContainers,
				Containers: []corev1.Container{
					{
						Name:    "node",
						Image:   "ipfs/go-ipfs:v0.6.0",
						Command: []string{"ipfs"},
						Args:    []string{"daemon"},
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      "data",
								MountPath: "/data/ipfs",
							},
						},
					},
				},
				Volumes: []corev1.Volume{
					{
						Name: "data",
						VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{},
						},
					},
				},
			},
		},
	}
}

// SetupWithManager registers the controller to be started with the given manager
func (r *SwarmReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ipfsv1alpha1.Swarm{}).
		Owns(&appsv1.Deployment{}).
		Complete(r)
}