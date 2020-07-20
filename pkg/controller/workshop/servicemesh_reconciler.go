package workshop

import (
	"context"
	"fmt"
	"strings"
	"time"

	openshiftv1alpha1 "github.com/redhat/openshift-workshop-operator/pkg/apis/openshift/v1alpha1"
	deployment "github.com/redhat/openshift-workshop-operator/pkg/deployment"
	smcp "github.com/redhat/openshift-workshop-operator/pkg/deployment/maistra/servicemeshcontrolplane"
	smmr "github.com/redhat/openshift-workshop-operator/pkg/deployment/maistra/servicemeshmemberroll"
	"github.com/redhat/openshift-workshop-operator/pkg/util"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Reconciling ServiceMesh
func (r *ReconcileWorkshop) reconcileServiceMesh(instance *openshiftv1alpha1.Workshop, users int) (reconcile.Result, error) {
	enabledServiceMesh := instance.Spec.Infrastructure.ServiceMesh.Enabled
	enabledServerless := instance.Spec.Infrastructure.Serverless.Enabled

	if enabledServiceMesh || enabledServerless {

		if result, err := r.addElasticSearchOperator(instance); err != nil {
			return result, err
		}

		if result, err := r.addJaegerOperator(instance); err != nil {
			return result, err
		}

		if result, err := r.addKialiOperator(instance); err != nil {
			return result, err
		}

		if result, err := r.addServiceMesh(instance, users); err != nil {
			return result, err
		}

		// Installed
		if instance.Status.ServiceMesh != util.OperatorStatus.Installed {
			instance.Status.ServiceMesh = util.OperatorStatus.Installed
			if err := r.client.Status().Update(context.TODO(), instance); err != nil {
				logrus.Errorf("Failed to update Workshop status: %s", err)
				return reconcile.Result{}, nil
			}
		}
	}

	//Success
	return reconcile.Result{}, nil
}

func (r *ReconcileWorkshop) addServiceMesh(instance *openshiftv1alpha1.Workshop, users int) (reconcile.Result, error) {

	// Service Mesh Operator
	channel := instance.Spec.Infrastructure.ServiceMesh.ServiceMeshOperatorHub.Channel
	clusterserviceversion := instance.Spec.Infrastructure.ServiceMesh.ServiceMeshOperatorHub.ClusterServiceVersion

	subscription := deployment.NewRedHatSubscription(instance, "servicemeshoperator", "openshift-operators",
		"servicemeshoperator", channel, clusterserviceversion)
	if err := r.client.Create(context.TODO(), subscription); err != nil && !errors.IsAlreadyExists(err) {
		return reconcile.Result{}, err
	} else if err == nil {
		logrus.Infof("Created %s Subscription", subscription.Name)
	}

	if err := r.ApproveInstallPlan(clusterserviceversion, "servicemeshoperator", "openshift-operators"); err != nil {
		logrus.Infof("Waiting for Subscription to create InstallPlan for %s", subscription.Name)
		return reconcile.Result{}, err
	}

	// Deploy Service Mesh
	istioSystemNamespace := deployment.NewNamespace(instance, "istio-system")
	if err := r.client.Create(context.TODO(), istioSystemNamespace); err != nil && !errors.IsAlreadyExists(err) {
		return reconcile.Result{}, err
	} else if err == nil {
		logrus.Infof("Created %s Namespace", istioSystemNamespace.Name)
	}

	serviceMeshControlPlaneCR := smcp.NewServiceMeshControlPlaneCR(smcp.NewServiceMeshControlPlaneCRParameters{
		Name:      "full-install",
		Namespace: istioSystemNamespace.Name,
	})
	if err := r.client.Create(context.TODO(), serviceMeshControlPlaneCR); err != nil && !errors.IsAlreadyExists(err) {
		return reconcile.Result{Requeue: true, RequeueAfter: time.Second * 1}, err
	} else if err == nil {
		logrus.Infof("Created %s Custom Resource", serviceMeshControlPlaneCR.Name)
	}

	istioMembers := []string{}
	for id := 1; id <= users; id++ {
		username := fmt.Sprintf("user%d", id)
		stagingProjectName := fmt.Sprintf("%s%d", instance.Spec.Infrastructure.Project.StagingName, id)

		istioMembers = append(istioMembers, stagingProjectName)

		jaegerRole := deployment.NewRole(deployment.NewRoleParameters{
			Name:      username + "-jaeger",
			Namespace: "istio-system",
			Rules:     deployment.JaegerUserRules(),
		})
		if err := r.client.Create(context.TODO(), jaegerRole); err != nil && !errors.IsAlreadyExists(err) {
			return reconcile.Result{}, err
		} else if err == nil {
			logrus.Infof("Created %s Role", jaegerRole.Name)
		}

		JaegerRoleBinding := deployment.NewRoleBindingUser(deployment.NewRoleBindingUserParameters{
			Name:      username + "-jaeger",
			Namespace: "istio-system",
			Username:  username,
			RoleName:  jaegerRole.Name,
			RoleKind:  "Role",
		})
		if err := r.client.Create(context.TODO(), JaegerRoleBinding); err != nil && !errors.IsAlreadyExists(err) {
			return reconcile.Result{}, err
		} else if err == nil {
			logrus.Infof("Created %s Role Binding", JaegerRoleBinding.Name)
		}
	}

	serviceMeshMemberRollCR := smmr.NewServiceMeshMemberRollCR(smmr.NewServiceMeshMemberRollCRParameters{
		Name:      "default",
		Namespace: istioSystemNamespace.Name,
		Members:   istioMembers,
	})
	if err := r.client.Create(context.TODO(), serviceMeshMemberRollCR); err != nil && !errors.IsAlreadyExists(err) {
		return reconcile.Result{}, err
	} else if err == nil {
		logrus.Infof("Created %s Custom Resource", serviceMeshMemberRollCR.Name)
	}

	// Updated Istio/Kiali label for the Workshop
	// Wait for Kiali to be running
	if !k8sclient.GetDeploymentStatus("kiali", istioSystemNamespace.Name) {
		return reconcile.Result{Requeue: true, RequeueAfter: time.Second * 1}, nil
	}

	kialiConfigMap, err := r.GetEffectiveConfigMap(instance, "kiali", istioSystemNamespace.Name)

	if err != nil {
		return reconcile.Result{}, err
	}

	configLines := strings.Split(kialiConfigMap.Data["config.yaml"], "\n")

	for i, line := range configLines {
		if strings.Contains(line, "app_label_name:") {
			configLines[i] = "  app_label_name: app.kubernetes.io/instance"
			break
		}
	}
	newConfig := strings.Join(configLines, "\n")

	if kialiConfigMap.Data["config.yaml"] != newConfig {
		kialiConfigMap.Data["config.yaml"] = newConfig
		if err := r.client.Update(context.TODO(), kialiConfigMap); err != nil {
			return reconcile.Result{}, err
		} else if err == nil {
			logrus.Infof("Updated Kiali ConfigMap for Labels")

			kialipodName, err := k8sclient.GetDeploymentPod("kiali", istioSystemNamespace.Name, "app")
			if err == nil {
				found := &corev1.Pod{}
				err = r.client.Get(context.TODO(), types.NamespacedName{Name: kialipodName, Namespace: istioSystemNamespace.Name}, found)
				if err == nil {
					if err := r.client.Delete(context.TODO(), found); err != nil {
						return reconcile.Result{}, err
					}
					logrus.Infof("Restarted a new Kiali Pod")
				}
			}
		}
	}

	// Updated Istio SideCar Injector for the Workshop
	injectorConfigMap, err := r.GetEffectiveConfigMap(instance, "istio-sidecar-injector", istioSystemNamespace.Name)

	if err != nil {
		return reconcile.Result{}, err
	}

	newConfig = strings.ReplaceAll(injectorConfigMap.Data["config"],
		"index .ObjectMeta.Labels \"app\"",
		"index .ObjectMeta.Labels \"deploymentconfig\"")

	if injectorConfigMap.Data["config"] != newConfig {
		injectorConfigMap.Data["config"] = newConfig
		if err := r.client.Update(context.TODO(), injectorConfigMap); err != nil {
			return reconcile.Result{}, err
		} else if err == nil {
			logrus.Infof("Updated %s ConfigMap", injectorConfigMap.Name)

			injectorPodName, err := k8sclient.GetDeploymentPod("sidecarInjectorWebhook", istioSystemNamespace.Name, "app")
			if err == nil {
				found := &corev1.Pod{}
				err = r.client.Get(context.TODO(), types.NamespacedName{Name: injectorPodName, Namespace: istioSystemNamespace.Name}, found)
				if err == nil {
					if err := r.client.Delete(context.TODO(), found); err != nil {
						return reconcile.Result{}, err
					}
					logrus.Infof("Restarted Istio Sidecar Injector Pod")
				}
			}
		}
	}

	//Success
	return reconcile.Result{}, nil
}

func (r *ReconcileWorkshop) addElasticSearchOperator(instance *openshiftv1alpha1.Workshop) (reconcile.Result, error) {

	channel := instance.Spec.Infrastructure.ServiceMesh.ElasticSearchOperatorHub.Channel
	clusterserviceversion := instance.Spec.Infrastructure.ServiceMesh.ElasticSearchOperatorHub.ClusterServiceVersion

	redhatOperatorsNamespace := deployment.NewNamespace(instance, "openshift-operators-redhat")
	if err := r.client.Create(context.TODO(), redhatOperatorsNamespace); err != nil && !errors.IsAlreadyExists(err) {
		return reconcile.Result{}, err
	} else if err == nil {
		logrus.Infof("Created %s Namespace", redhatOperatorsNamespace.Name)
	}

	subscription := deployment.NewRedHatSubscription(instance, "elasticsearch-operator", "openshift-operators-redhat",
		"elasticsearch-operator", channel, clusterserviceversion)
	if err := r.client.Create(context.TODO(), subscription); err != nil && !errors.IsAlreadyExists(err) {
		return reconcile.Result{}, err
	} else if err == nil {
		logrus.Infof("Created %s Subscription", subscription.Name)
	}

	if err := r.ApproveInstallPlan(clusterserviceversion, "elasticsearch-operator", "openshift-operators-redhat"); err != nil {
		logrus.Infof("Waiting for Subscription to create InstallPlan for %s", subscription.Name)
		return reconcile.Result{}, err
	}

	//Success
	return reconcile.Result{}, nil
}

func (r *ReconcileWorkshop) addJaegerOperator(instance *openshiftv1alpha1.Workshop) (reconcile.Result, error) {

	channel := instance.Spec.Infrastructure.ServiceMesh.JaegerOperatorHub.Channel
	clusterserviceversion := instance.Spec.Infrastructure.ServiceMesh.JaegerOperatorHub.ClusterServiceVersion

	subscription := deployment.NewRedHatSubscription(instance, "jaeger-product", "openshift-operators",
		"jaeger-product", channel, clusterserviceversion)
	if err := r.client.Create(context.TODO(), subscription); err != nil && !errors.IsAlreadyExists(err) {
		return reconcile.Result{}, err
	} else if err == nil {
		logrus.Infof("Created %s Subscription", subscription.Name)
	}

	if err := r.ApproveInstallPlan(clusterserviceversion, "jaeger-product", "openshift-operators"); err != nil {
		logrus.Infof("Waiting for Subscription to create InstallPlan for %s", subscription.Name)
		return reconcile.Result{}, err
	}

	//Success
	return reconcile.Result{}, nil
}

func (r *ReconcileWorkshop) addKialiOperator(instance *openshiftv1alpha1.Workshop) (reconcile.Result, error) {

	channel := instance.Spec.Infrastructure.ServiceMesh.KialiOperatorHub.Channel
	clusterserviceversion := instance.Spec.Infrastructure.ServiceMesh.KialiOperatorHub.ClusterServiceVersion

	subscription := deployment.NewRedHatSubscription(instance, "kiali-ossm", "openshift-operators",
		"kiali-ossm", channel, clusterserviceversion)
	if err := r.client.Create(context.TODO(), subscription); err != nil && !errors.IsAlreadyExists(err) {
		return reconcile.Result{}, err
	} else if err == nil {
		logrus.Infof("Created %s Subscription", subscription.Name)
	}

	if err := r.ApproveInstallPlan(clusterserviceversion, "kiali-ossm", "openshift-operators"); err != nil {
		logrus.Infof("Waiting for Subscription to create InstallPlan for %s", subscription.Name)
		return reconcile.Result{}, err
	}

	//Success
	return reconcile.Result{}, nil
}
