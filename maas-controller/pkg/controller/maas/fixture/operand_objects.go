package fixture

import (
	appsv1 "k8s.io/api/apps/v1"
	autov2 "k8s.io/api/autoscaling/v2"
	batcv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

// The Operand* builders return the smallest valid object of each built-in kind the
// tenant platform pipeline applies, without tenant tracking labels.

func operandPodSpec() corev1.PodSpec {
	return corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "registry.example.com/operand:1"}}}
}

// OperandDeployment returns a single-replica Deployment.
func OperandDeployment(namespace, name string) *appsv1.Deployment {
	selector := map[string]string{"app": name}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: selector},
				Spec:       operandPodSpec(),
			},
		},
	}
}

// OperandService returns a ClusterIP Service.
func OperandService(namespace, name string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": name},
			Ports:    []corev1.ServicePort{{Name: "https", Port: 8443, Protocol: corev1.ProtocolTCP}},
		},
	}
}

// OperandServiceAccount returns a ServiceAccount.
func OperandServiceAccount(namespace, name string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
}

// OperandConfigMap returns a ConfigMap with one key.
func OperandConfigMap(namespace, name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       map[string]string{"namespace": namespace},
	}
}

// OperandClusterRole returns a ClusterRole granting read access to ConfigMaps, carrying
// the ODH component label postBuildTransform stamps on every rendered object. The
// ClusterRole cache scope selects on that label (see operandSelector), so a ClusterRole
// fixture without it would never enter the informer the tests exercise.
func OperandClusterRole(name string) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{tenantreconcile.LabelODHAppPrefix + "/" + tenantreconcile.ComponentName: "true"},
		},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"configmaps"},
			Verbs:     []string{"get"},
		}},
	}
}

// OperandClusterRoleBinding binds the ClusterRole of the same name to a ServiceAccount.
func OperandClusterRoleBinding(name, serviceAccountNamespace string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      name,
			Namespace: serviceAccountNamespace,
		}},
	}
}

// OperandCronJob returns an hourly CronJob.
func OperandCronJob(namespace, name string) *batcv1.CronJob {
	podSpec := operandPodSpec()
	podSpec.RestartPolicy = corev1.RestartPolicyOnFailure
	return &batcv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: batcv1.CronJobSpec{
			Schedule: "0 * * * *",
			JobTemplate: batcv1.JobTemplateSpec{
				Spec: batcv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: podSpec}},
			},
		},
	}
}

// OperandHPA returns an HPA scaling the named Deployment on CPU.
func OperandHPA(namespace, name, deployment string) *autov2.HorizontalPodAutoscaler {
	return &autov2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: autov2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autov2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: deployment},
			MinReplicas:    ptr.To[int32](1),
			MaxReplicas:    3,
			Metrics: []autov2.MetricSpec{{
				Type: autov2.ResourceMetricSourceType,
				Resource: &autov2.ResourceMetricSource{
					Name:   corev1.ResourceCPU,
					Target: autov2.MetricTarget{Type: autov2.UtilizationMetricType, AverageUtilization: ptr.To[int32](70)},
				},
			}},
		},
	}
}
