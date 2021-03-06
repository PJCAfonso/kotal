package controllers

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ethereumv1alpha1 "github.com/kotalco/kotal/apis/ethereum/v1alpha1"
	"github.com/kotalco/kotal/helpers"
)

// NetworkReconciler reconciles a Network object
type NetworkReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=ethereum.kotal.io,resources=networks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ethereum.kotal.io,resources=networks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=watch;get;list;create;update;delete
// +kubebuilder:rbac:groups=core,resources=secrets;services;configmaps;persistentvolumeclaims,verbs=watch;get;create;update;list;delete

// Reconcile reconciles ethereum networks
func (r *NetworkReconciler) Reconcile(req ctrl.Request) (result ctrl.Result, err error) {
	var _ = context.Background()

	var network ethereumv1alpha1.Network

	// Get desired ethereum network
	if err = r.Client.Get(context.Background(), req.NamespacedName, &network); err != nil {
		err = client.IgnoreNotFound(err)
		return
	}

	// update network status
	if err = r.updateStatus(&network); err != nil {
		return
	}

	// reconcile network nodes
	if err = r.reconcileNodes(&network); err != nil {
		return
	}

	return

}

// updateStatus updates network status
// TODO: don't update statuse on network deletion
func (r *NetworkReconciler) updateStatus(network *ethereumv1alpha1.Network) error {
	network.Status.NodesCount = len(network.Spec.Nodes)

	if err := r.Status().Update(context.Background(), network); err != nil {
		r.Log.Error(err, "unable to update network status")
		return err
	}

	return nil
}

// reconcileNodes creates or updates nodes according to nodes spec
// deletes nodes missing from nodes spec
func (r *NetworkReconciler) reconcileNodes(network *ethereumv1alpha1.Network) error {
	bootnodes := []string{}

	for _, node := range network.Spec.Nodes {

		bootnode, err := r.reconcileNode(&node, network, bootnodes)
		if err != nil {
			return err
		}

		if node.IsBootnode() {
			bootnodes = append(bootnodes, bootnode)
		}

	}

	if err := r.deleteRedundantNodes(network); err != nil {
		return err
	}

	return nil
}

// specNodeConfigmap updates genesis configmap spec
func (r *NetworkReconciler) specNodeConfigmap(configmap *corev1.ConfigMap, genesis, initGenesisScript, importAccountScript string) {
	configmap.Data = make(map[string]string)
	configmap.Data["genesis.json"] = genesis
	configmap.Data["init-genesis.sh"] = initGenesisScript
	configmap.Data["import-account.sh"] = importAccountScript
}

// reconcileNodeConfigmap creates genesis config map if it doesn't exist or update it
func (r *NetworkReconciler) reconcileNodeConfigmap(node *ethereumv1alpha1.Node, network *ethereumv1alpha1.Network) error {

	configmap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      node.ConfigmapName(network.Name, node.Client),
			Namespace: network.Namespace,
		},
	}

	var genesis, initGenesisScript, importAccountScript string

	// no genesis or init scripts are required for besu clients in public networks
	if network.Spec.Genesis == nil && node.Client == ethereumv1alpha1.BesuClient {
		return nil
	}

	// private network with custom genesis
	if network.Spec.Genesis != nil {
		client, err := NewEthereumClient(node.Client)
		if err != nil {
			return err
		}
		// create client specific genesis configuration
		if genesis, err = client.GetGenesisFile(network.Spec.Genesis, network.Spec.Consensus); err != nil {
			return err
		}
		// create init genesis script if client is geth
		if node.Client == ethereumv1alpha1.GethClient {
			initGenesisScript, err = generateInitGenesisScript()
			if err != nil {
				return err
			}
		}
	}

	// geth only
	// create import account script
	if node.Import != nil {
		var err error
		importAccountScript, err = generateImportAccountScript()
		if err != nil {
			return err
		}
	}

	_, err := ctrl.CreateOrUpdate(context.Background(), r.Client, configmap, func() error {
		if err := ctrl.SetControllerReference(network, configmap, r.Scheme); err != nil {
			r.Log.Error(err, "Unable to set controller reference on genesis configmap")
			return err
		}

		r.specNodeConfigmap(configmap, genesis, initGenesisScript, importAccountScript)

		return nil
	})

	return err
}

// deleteRedundantNode deletes all nodes that has been removed from spec
// network is the owner of the redundant resources (node deployment, svc, secret and pvc)
// removing nodes from spec won't remove these resources by grabage collection
// that's why we're deleting them manually
func (r *NetworkReconciler) deleteRedundantNodes(network *ethereumv1alpha1.Network) error {
	log := r.Log.WithName("delete redundant nodes")

	var deps appsv1.DeploymentList
	var pvcs corev1.PersistentVolumeClaimList
	var secrets corev1.SecretList
	var services corev1.ServiceList

	nodes := network.Spec.Nodes
	names := map[string]bool{}
	matchingLabels := client.MatchingLabels{
		"name":    "node",
		"network": network.Name,
	}
	inNamespace := client.InNamespace(network.Namespace)

	for _, node := range nodes {
		depName := node.DeploymentName(network.Name)
		names[depName] = true
	}

	// Node deployments
	if err := r.Client.List(context.Background(), &deps, matchingLabels, inNamespace); err != nil {
		log.Error(err, "unable to list all node deployments")
		return err
	}

	for _, dep := range deps.Items {
		name := dep.GetName()
		if exist := names[name]; !exist {
			log.Info(fmt.Sprintf("deleting node (%s) deployment", name))

			if err := r.Client.Delete(context.Background(), &dep); err != nil {
				log.Error(err, fmt.Sprintf("unable to delete node (%s) deployment", name))
				return err
			}
		}
	}

	// Node PVCs
	if err := r.Client.List(context.Background(), &pvcs, matchingLabels, inNamespace); err != nil {
		log.Error(err, "unable to list all node pvcs")
		return err
	}

	for _, pvc := range pvcs.Items {
		name := pvc.GetName()
		if exist := names[name]; !exist {
			log.Info(fmt.Sprintf("deleting node (%s) pvc", name))

			if err := r.Client.Delete(context.Background(), &pvc); err != nil {
				log.Error(err, fmt.Sprintf("unable to delete node (%s) pvc", name))
				return err
			}
		}
	}

	// Node Secrets
	if err := r.Client.List(context.Background(), &secrets, matchingLabels, inNamespace); err != nil {
		log.Error(err, "unable to list all node secrets")
		return err
	}

	for _, secret := range secrets.Items {
		name := secret.GetName()
		if exist := names[name]; !exist {
			log.Info(fmt.Sprintf("deleting node (%s) secret", name))

			if err := r.Client.Delete(context.Background(), &secret); err != nil {
				log.Error(err, fmt.Sprintf("unable to delete node (%s) secret", name))
				return err
			}
		}
	}

	// Node Services
	if err := r.Client.List(context.Background(), &services, matchingLabels, inNamespace); err != nil {
		log.Error(err, "unable to list all node services")
		return err
	}

	for _, service := range services.Items {
		name := service.GetName()
		if exist := names[name]; !exist {
			log.Info(fmt.Sprintf("deleting node (%s) service", name))

			if err := r.Client.Delete(context.Background(), &service); err != nil {
				log.Error(err, fmt.Sprintf("unable to delete node (%s) service", name))
				return err
			}
		}
	}

	return nil
}

// specNodeDataPVC update node data pvc spec
func (r *NetworkReconciler) specNodeDataPVC(pvc *corev1.PersistentVolumeClaim, node *ethereumv1alpha1.Node, network *ethereumv1alpha1.Network) {
	pvc.ObjectMeta.Labels = node.Labels(network.Name)
	pvc.Spec = corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{
			corev1.ReadWriteOnce,
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse(node.Resources.Storage),
			},
		},
		StorageClassName: node.Resources.StorageClass,
	}
}

// reconcileNodeDataPVC creates node data pvc if it doesn't exist
func (r *NetworkReconciler) reconcileNodeDataPVC(node *ethereumv1alpha1.Node, network *ethereumv1alpha1.Network) error {

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      node.PVCName(network.Name),
			Namespace: network.Namespace,
		},
	}

	_, err := ctrl.CreateOrUpdate(context.Background(), r.Client, pvc, func() error {
		if err := ctrl.SetControllerReference(network, pvc, r.Scheme); err != nil {
			return err
		}
		if pvc.CreationTimestamp.IsZero() {
			r.specNodeDataPVC(pvc, node, network)
		}
		return nil
	})

	return err
}

// createNodeVolumes creates all the required volumes for the node
func (r *NetworkReconciler) createNodeVolumes(node *ethereumv1alpha1.Node, network *ethereumv1alpha1.Network) []corev1.Volume {

	volumes := []corev1.Volume{}

	if node.WithNodekey() || node.Import != nil {
		nodekeyVolume := corev1.Volume{
			Name: "secrets",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: node.SecretName(network.Name),
				},
			},
		}
		volumes = append(volumes, nodekeyVolume)
	}

	if network.Spec.Genesis != nil {
		genesisVolume := corev1.Volume{
			Name: "config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: node.ConfigmapName(network.Name, node.Client),
					},
				},
			},
		}
		volumes = append(volumes, genesisVolume)
	}

	dataVolume := corev1.Volume{
		Name: "data",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: node.PVCName(network.Name),
			},
		},
	}
	volumes = append(volumes, dataVolume)

	return volumes
}

// createNodeVolumeMounts creates all required volume mounts for the node
func (r *NetworkReconciler) createNodeVolumeMounts(node *ethereumv1alpha1.Node, network *ethereumv1alpha1.Network) []corev1.VolumeMount {

	volumeMounts := []corev1.VolumeMount{}

	if node.WithNodekey() || node.Import != nil {
		nodekeyMount := corev1.VolumeMount{
			Name:      "secrets",
			MountPath: PathSecrets,
			ReadOnly:  true,
		}
		volumeMounts = append(volumeMounts, nodekeyMount)
	}

	if network.Spec.Genesis != nil {
		genesisMount := corev1.VolumeMount{
			Name:      "config",
			MountPath: PathConfig,
			ReadOnly:  true,
		}
		volumeMounts = append(volumeMounts, genesisMount)
	}

	dataMount := corev1.VolumeMount{
		Name:      "data",
		MountPath: PathBlockchainData,
	}
	volumeMounts = append(volumeMounts, dataMount)

	return volumeMounts
}

// getNodeAffinity returns affinity settings to be use by the node pod
func (r *NetworkReconciler) getNodeAffinity(network *ethereumv1alpha1.Network) *corev1.Affinity {
	if network.Spec.HighlyAvailable {
		return &corev1.Affinity{
			PodAntiAffinity: &corev1.PodAntiAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
					{
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"name":    "node",
								"network": network.Name,
							},
						},
						TopologyKey: network.Spec.TopologyKey,
					},
				},
			},
		}
	}
	return nil
}

// specNodeDeployment updates node deployment spec
func (r *NetworkReconciler) specNodeDeployment(dep *appsv1.Deployment, node *ethereumv1alpha1.Node, network *ethereumv1alpha1.Network, args []string, volumes []corev1.Volume, volumeMounts []corev1.VolumeMount, affinity *corev1.Affinity) {
	labels := node.Labels(network.Name)
	// used by geth to init genesis and import account(s)
	initContainers := []corev1.Container{}
	// node client container
	nodeContainer := corev1.Container{
		Name: "node",
		Args: args,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(node.Resources.CPU),
				corev1.ResourceMemory: resource.MustParse(node.Resources.Memory),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(node.Resources.CPULimit),
				corev1.ResourceMemory: resource.MustParse(node.Resources.MemoryLimit),
			},
		},
		VolumeMounts: volumeMounts,
	}

	if node.Client == ethereumv1alpha1.GethClient {
		if network.Spec.Genesis != nil {
			initGenesis := corev1.Container{
				Name:         "init-genesis",
				Image:        GethImage(),
				Command:      []string{"/bin/sh"},
				Args:         []string{fmt.Sprintf("%s/init-genesis.sh", PathConfig)},
				VolumeMounts: volumeMounts,
			}
			initContainers = append(initContainers, initGenesis)
		}
		if node.Import != nil {
			importAccount := corev1.Container{
				Name:         "import-account",
				Image:        GethImage(),
				Command:      []string{"/bin/sh"},
				Args:         []string{fmt.Sprintf("%s/import-account.sh", PathConfig)},
				VolumeMounts: volumeMounts,
			}
			initContainers = append(initContainers, importAccount)
		}

		nodeContainer.Image = GethImage()
		nodeContainer.Command = []string{"geth"}

	} else if node.Client == ethereumv1alpha1.BesuClient {
		nodeContainer.Image = BesuImage()
		nodeContainer.Command = []string{"besu"}
	}

	dep.ObjectMeta.Labels = labels
	if dep.Spec.Selector == nil {
		dep.Spec.Selector = &metav1.LabelSelector{}
	}
	dep.Spec.Selector.MatchLabels = labels
	dep.Spec.Template.ObjectMeta.Labels = labels
	dep.Spec.Template.Spec = corev1.PodSpec{
		Volumes:        volumes,
		InitContainers: initContainers,
		Containers:     []corev1.Container{nodeContainer},
		Affinity:       affinity,
	}
}

// reconcileNodeDeployment creates creates node deployment if it doesn't exist, update it if it does exist
func (r *NetworkReconciler) reconcileNodeDeployment(node *ethereumv1alpha1.Node, network *ethereumv1alpha1.Network, bootnodes []string) error {

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      node.DeploymentName(network.Name),
			Namespace: network.Namespace,
		},
	}

	client, err := NewEthereumClient(node.Client)
	if err != nil {
		return err
	}
	args := client.GetArgs(node, network, bootnodes)
	volumes := r.createNodeVolumes(node, network)
	mounts := r.createNodeVolumeMounts(node, network)
	affinity := r.getNodeAffinity(network)

	_, err = ctrl.CreateOrUpdate(context.Background(), r.Client, dep, func() error {
		if err := ctrl.SetControllerReference(network, dep, r.Scheme); err != nil {
			return err
		}
		r.specNodeDeployment(dep, node, network, args, volumes, mounts, affinity)
		return nil
	})

	return err
}

func (r *NetworkReconciler) specNodeSecret(secret *corev1.Secret, node *ethereumv1alpha1.Node, network *ethereumv1alpha1.Network) {
	secret.ObjectMeta.Labels = node.Labels(network.Name)
	data := map[string]string{}

	if node.WithNodekey() {
		data["nodekey"] = string(node.Nodekey)[2:]
	}

	if node.Import != nil {
		data["account.key"] = string(node.Import.PrivateKey)[2:]
		data["account.password"] = node.Import.Password
	}

	secret.StringData = data
}

// reconcileNodeSecret creates node secret if it doesn't exist, update it if it exists
func (r *NetworkReconciler) reconcileNodeSecret(node *ethereumv1alpha1.Node, network *ethereumv1alpha1.Network) (publicKey string, err error) {

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      node.SecretName(network.Name),
			Namespace: network.Namespace,
		},
	}

	if node.WithNodekey() {
		// hex private key without the leading 0x
		privateKey := string(node.Nodekey)[2:]
		publicKey, err = helpers.DerivePublicKey(privateKey)
		if err != nil {
			return
		}
	}

	_, err = ctrl.CreateOrUpdate(context.Background(), r.Client, secret, func() error {
		if err := ctrl.SetControllerReference(network, secret, r.Scheme); err != nil {
			return err
		}

		r.specNodeSecret(secret, node, network)

		return nil
	})

	if err != nil {
		return
	}

	return
}

// specNodeService updates node service spec
func (r *NetworkReconciler) specNodeService(svc *corev1.Service, node *ethereumv1alpha1.Node, network *ethereumv1alpha1.Network) {
	labels := node.Labels(network.Name)
	svc.ObjectMeta.Labels = labels
	svc.Spec.Ports = []corev1.ServicePort{
		{
			Name:       "discovery",
			Port:       int32(node.P2PPort),
			TargetPort: intstr.FromInt(int(node.P2PPort)),
			Protocol:   corev1.ProtocolUDP,
		},
		{
			Name:       "p2p",
			Port:       int32(node.P2PPort),
			TargetPort: intstr.FromInt(int(node.P2PPort)),
			Protocol:   corev1.ProtocolTCP,
		},
	}

	svc.Spec.Selector = labels
}

// reconcileNodeService reconciles node service
func (r *NetworkReconciler) reconcileNodeService(node *ethereumv1alpha1.Node, network *ethereumv1alpha1.Network) (ip string, err error) {

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      node.ServiceName(network.Name),
			Namespace: network.Namespace,
		},
	}

	_, err = ctrl.CreateOrUpdate(context.Background(), r.Client, svc, func() error {
		if err = ctrl.SetControllerReference(network, svc, r.Scheme); err != nil {
			return err
		}

		r.specNodeService(svc, node, network)

		return nil
	})

	if err != nil {
		return
	}

	ip = svc.Spec.ClusterIP

	return
}

// reconcileNode create a new node deployment if it doesn't exist
// updates existing deployments if node spec changed
func (r *NetworkReconciler) reconcileNode(node *ethereumv1alpha1.Node, network *ethereumv1alpha1.Network, bootnodes []string) (enodeURL string, err error) {

	if err = r.reconcileNodeDataPVC(node, network); err != nil {
		return
	}

	if err = r.reconcileNodeConfigmap(node, network); err != nil {
		return
	}

	if err = r.reconcileNodeDeployment(node, network, bootnodes); err != nil {
		return
	}

	if !node.WithNodekey() && node.Import == nil {
		return
	}

	var publicKey string

	if publicKey, err = r.reconcileNodeSecret(node, network); err != nil {
		return
	}

	if !node.IsBootnode() {
		return
	}

	ip, err := r.reconcileNodeService(node, network)
	if err != nil {
		return
	}

	enodeURL = fmt.Sprintf("enode://%s@%s:%d", publicKey, ip, node.P2PPort)

	return
}

// SetupWithManager adds reconciler to the manager
func (r *NetworkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ethereumv1alpha1.Network{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&corev1.ConfigMap{}).
		Complete(r)
}
