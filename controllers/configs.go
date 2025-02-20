// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package controllers

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"time"

	cabptv1 "github.com/talos-systems/cluster-api-bootstrap-provider-talos/api/v1alpha3"
	controlplanev1 "github.com/talos-systems/cluster-api-control-plane-provider-talos/api/v1alpha3"
	talosclient "github.com/talos-systems/talos/pkg/machinery/client"
	talosconfig "github.com/talos-systems/talos/pkg/machinery/client/config"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/connrotation"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type kubernetesClient struct {
	*kubernetes.Clientset

	dialer *connrotation.Dialer
}

// Close kubernetes client.
func (k *kubernetesClient) Close() error {
	k.dialer.CloseAll()

	return nil
}

func newDialer() *connrotation.Dialer {
	return connrotation.NewDialer((&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext)
}

// kubeconfigForCluster will fetch a kubeconfig secret based on cluster name/namespace,
// use it to create a clientset, and return it.
func (r *TalosControlPlaneReconciler) kubeconfigForCluster(ctx context.Context, cluster client.ObjectKey) (*kubernetesClient, error) {
	kubeconfigSecret := &corev1.Secret{}

	err := r.Client.Get(ctx,
		types.NamespacedName{
			Namespace: cluster.Namespace,
			Name:      cluster.Name + "-kubeconfig",
		},
		kubeconfigSecret,
	)
	if err != nil {
		return nil, err
	}

	config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigSecret.Data["value"])
	if err != nil {
		return nil, err
	}

	dialer := newDialer()
	config.Dial = dialer.DialContext

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return &kubernetesClient{
		Clientset: clientset,
		dialer:    dialer,
	}, nil
}

// talosconfigForMachine will generate a talosconfig that uses *all* found addresses as the endpoints.
func (r *TalosControlPlaneReconciler) talosconfigForMachines(ctx context.Context, tcp *controlplanev1.TalosControlPlane, machines ...clusterv1.Machine) (*talosclient.Client, error) {
	if len(machines) == 0 {
		return nil, fmt.Errorf("at least one machine should be provided")
	}

	if !reflect.ValueOf(tcp.Spec.ControlPlaneConfig.InitConfig).IsZero() {
		return r.talosconfigFromWorkloadCluster(ctx, client.ObjectKey{Namespace: tcp.GetNamespace(), Name: tcp.GetLabels()["cluster.x-k8s.io/cluster-name"]}, machines...)
	}

	addrList := []string{}

	var t *talosconfig.Config

	for _, machine := range machines {
		for _, addr := range machine.Status.Addresses {
			if addr.Type == clusterv1.MachineExternalIP || addr.Type == clusterv1.MachineInternalIP {
				addrList = append(addrList, addr.Address)
			}
		}

		if len(addrList) == 0 {
			return nil, fmt.Errorf("no addresses were found for node %q", machine.Name)
		}

		if t == nil {
			var (
				cfgs  cabptv1.TalosConfigList
				found *cabptv1.TalosConfig
			)

			// find talosconfig in the machine's namespace
			err := r.Client.List(ctx, &cfgs, client.InNamespace(machine.Namespace))
			if err != nil {
				return nil, err
			}

		outer:
			for _, cfg := range cfgs.Items {
				for _, ref := range cfg.OwnerReferences {
					if ref.Kind == "Machine" && ref.Name == machine.Name {
						found = &cfg
						break outer
					}
				}
			}

			if found == nil {
				return nil, fmt.Errorf("failed to find TalosConfig for %q", machine.Name)
			}

			t, err = talosconfig.FromString(found.Status.TalosConfig)
			if err != nil {
				return nil, err
			}
		}
	}

	return talosclient.New(ctx, talosclient.WithEndpoints(addrList...), talosclient.WithConfig(t))
}

// talosconfigFromWorkloadCluster gets talosconfig and populates endoints using workload cluster nodes.
func (r *TalosControlPlaneReconciler) talosconfigFromWorkloadCluster(ctx context.Context, cluster client.ObjectKey, machines ...clusterv1.Machine) (*talosclient.Client, error) {
	if len(machines) == 0 {
		return nil, fmt.Errorf("at least one machine should be provided")
	}

	clientset, err := r.kubeconfigForCluster(ctx, cluster)
	if err != nil {
		return nil, err
	}

	addrList := []string{}

	var t *talosconfig.Config

	for _, machine := range machines {
		if machine.Status.NodeRef == nil {
			return nil, fmt.Errorf("%q machine does not have a nodeRef", machine.Name)
		}

		// grab all addresses as endpoints
		node, err := clientset.CoreV1().Nodes().Get(ctx, machine.Status.NodeRef.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}

		for _, addr := range node.Status.Addresses {
			if addr.Type == corev1.NodeExternalIP || addr.Type == corev1.NodeInternalIP {
				addrList = append(addrList, addr.Address)
			}
		}

		if len(addrList) == 0 {
			return nil, fmt.Errorf("no addresses were found for node %q", node.Name)
		}

		if t == nil {
			var (
				cfgs  cabptv1.TalosConfigList
				found *cabptv1.TalosConfig
			)

			// find talosconfig in the machine's namespace
			err = r.Client.List(ctx, &cfgs, client.InNamespace(machine.Namespace))
			if err != nil {
				return nil, err
			}

			for _, cfg := range cfgs.Items {
				for _, ref := range cfg.OwnerReferences {
					if ref.Kind == "Machine" && ref.Name == machine.Name {
						found = &cfg
						break
					}
				}
			}

			if found == nil {
				return nil, fmt.Errorf("failed to find TalosConfig for %q", machine.Name)
			}

			t, err = talosconfig.FromString(found.Status.TalosConfig)
			if err != nil {
				return nil, err
			}
		}
	}

	return talosclient.New(ctx, talosclient.WithEndpoints(addrList...), talosclient.WithConfig(t))
}
