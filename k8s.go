package main

import (
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	defaultHTTPHandlerName = "default-http-handler"
	systemNamespace        = "glb-ingress-controller"
)

func defaultHTTPHandlerDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      defaultHTTPHandlerName,
			Namespace: systemNamespace,
			Labels: map[string]string{
				"name": defaultHTTPHandlerName,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"name": defaultHTTPHandlerName,
				},
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"name": defaultHTTPHandlerName,
					},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:  defaultHTTPHandlerName,
							Image: "ssttevee/default-http-handler:go",
							ReadinessProbe: &v1.Probe{
								Handler: v1.Handler{
									HTTPGet: &v1.HTTPGetAction{
										Path: "/healthz",
										Port: intstr.FromInt(80),
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func defaultHTTPHandlerService() *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      defaultHTTPHandlerName,
			Namespace: systemNamespace,
		},
		Spec: v1.ServiceSpec{
			Selector: map[string]string{
				"name": defaultHTTPHandlerName,
			},
			Ports: []v1.ServicePort{
				{
					Protocol: v1.ProtocolTCP,
					Name: "http",
					Port: 80,
					TargetPort: intstr.FromInt(80),
				},
			},
			Type: v1.ServiceTypeNodePort,
		},
	}
}
