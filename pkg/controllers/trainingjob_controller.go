/*

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

package controllers

import (
	"context"
	"fmt"

	kaiv1alpha1 "github.com/AliyunContainerService/et-operator/api/v1alpha1"
	common "github.com/AliyunContainerService/et-operator/pkg/controllers/api/v1"
	commonv1 "github.com/AliyunContainerService/et-operator/pkg/controllers/api/v1"
	"github.com/go-logr/logr"
	logger "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	k8scontroller "k8s.io/kubernetes/pkg/controller"

	"reflect"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	//"sigs.k8s.io/controller-runtime/pkg/predicate"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	controllerName          = "trainingjob"
	configSuffix            = "-config"
	launcherSuffix          = "-launcher"
	workerSuffix            = "-worker"
	kubectlVolumeName       = "training-job-kubectl"
	configVolumeName        = "training-job-config"
	configMountPath         = "/etc/mpi"
	hostfileName            = "hostfile"
	kubexeclFileName        = "kubexec.sh"
	discoverHostName        = "discover_hosts.sh"
	kubectlMountPath        = "/opt/kube"
	labelGroupName          = "group-name"
	labelTrainingJobName    = "training-job-name"
	labelTrainingRoleType   = "training-job-role"
	launcher                = "launcher"
	worker                  = "worker"
	initContainerCpu        = "100m"
	initContainerEphStorage = "5Gi"
	initContainerMem        = "512Mi"
	replicaIndexLabel       = "replica-index"
	gpuResourceName         = "nvidia.com/gpu"
	initContainerImage      = "alpine:3.10"
	initContainerName       = "init-hostfile"
	hostfileVolumeName      = "training-job-hostfile"
	hostfileMountPath       = "/etc/edl"

	// DeepSpeed hostfile
	deepSpeedMountPath    = "/job"
	deepSpeedHostfileName = "deepspeed-hostfile"
)

const (
	// ErrResourceExists is used as part of the Event 'reason' when an MPIJob
	// fails to sync due to dependent resources of the same name already
	// existing.
	ErrResourceExists = "ErrResourceExists"

	// MessageResourceExists is the message used for Events when a resource
	// fails to sync due to dependent resources already existing.
	MessageResourceExists = "Resource %q of Kind %q already exists and is not managed by MPIJob"

	// ErrResourceDoesNotExist is used as part of the Event 'reason' when some
	// resource is missing in yaml
	ErrResourceDoesNotExist = "ErrResourceDoesNotExist"

	// MessageResourceDoesNotExist is used for Events when some
	// resource is missing in yaml
	MessageResourceDoesNotExist = "Resource %q is missing in yaml"

	// podTemplateRestartPolicyReason is the warning reason when the restart
	// policy is set in pod template.
	podTemplateRestartPolicyReason = "SettedPodTemplateRestartPolicy"
)

func NewTrainingJobController(controllerImpl *TrainingJobReconciler) TrainingJobController {
	return TrainingJobController{
		BackoffStatesQueue: workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		//Controller:         controllerImpl,
		Expectations: k8scontroller.NewControllerExpectations(),
	}
}

func NewReconciler(mgr ctrl.Manager, pollInterval time.Duration, enableCreateSecret bool) *TrainingJobReconciler {
	r := &TrainingJobReconciler{
		Client:             mgr.GetClient(),
		Scheme:             mgr.GetScheme(),
		PollInterval:       pollInterval,
		Log:                ctrl.Log.WithName("controllers").WithName("TrainingJob"),
		EnableCreateSecret: enableCreateSecret,
	}
	r.recorder = mgr.GetEventRecorderFor(controllerName)
	//r.ctrl = NewTrainingJobController(r)
	return r
}

var (
	_ reconcile.Reconciler = &TrainingJobReconciler{}
	//_ ControllerInterface  = &TrainingJobReconciler{}
)

// TrainingJobReconciler reconciles a TrainingJob object
type TrainingJobReconciler struct {
	client.Client
	Log                logr.Logger
	recorder           record.EventRecorder
	Scheme             *runtime.Scheme
	ctrl               TrainingJobController
	PollInterval       time.Duration
	EnableCreateSecret bool
}

func (r *TrainingJobReconciler) ControllerName() string {
	return controllerName
}

type TrainingJobController struct {
	kubectlDeliveryImage string
	// BackoffStatesQueue is a rate limited queue and record backoff counts for
	// those reconciling-failed job instances, and it does not play a role of
	// build-in work queue in controller-runtime.
	BackoffStatesQueue workqueue.RateLimitingInterface
	Controller         ControllerInterface
	Expectations       k8scontroller.ControllerExpectationsInterface
}

// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="rbac.authorization.k8s.io",resources=roles,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="rbac.authorization.k8s.io",resources=rolebindings,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create
// +kubebuilder:rbac:groups="",resources=pods/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kai.alibabacloud.com,resources=trainingjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kai.alibabacloud.com,resources=trainingjobs/status,verbs=get;update;patch

func (r *TrainingJobReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {

	rlog := r.Log.WithValues("trainingjob", req.NamespacedName)
	// Fetch latest training job instance.
	sharedTrainingJob := &kaiv1alpha1.TrainingJob{}
	err := r.Get(context.Background(), req.NamespacedName, sharedTrainingJob)
	if err != nil {
		if errors.IsNotFound(err) {
			return NoRequeue()
		}
		rlog.Info("Unable to fetch TrainingJob job", "reason", err)
		// Error reading the object - requeue the request.
		return RequeueImmediately()
	}
	trainingJob := sharedTrainingJob.DeepCopy()
	// Check reconcile is required.
	// No need to do reconcile or job has been deleted.
	if trainingJob.DeletionTimestamp != nil {
		rlog.Info("reconcile cancelled, job does not need to do reconcile or has been deleted")
		return NoRequeue()
	}

	r.Scheme.Default(trainingJob)

	return r.ReconcileJobs(trainingJob)
}

func GenLabels(jobName string) map[string]string {
	return map[string]string{
		labelGroupName:       kaiv1alpha1.GroupVersion.Group,
		labelTrainingJobName: jobName,
	}
}

func (r *TrainingJobReconciler) ReconcileJobs(job *kaiv1alpha1.TrainingJob) (result reconcile.Result, err error) {
	oldJobStatus := job.Status.DeepCopy()
	logger.Infof("Reconcile job %s/%s for %s", job.Namespace, job.Name, job.Status.Phase)

	defer func() {
		latestJob := &kaiv1alpha1.TrainingJob{}
		err := r.Get(context.Background(), types.NamespacedName{
			Name:      job.Name,
			Namespace: job.Namespace,
		}, latestJob)
		if err == nil {
			if latestJob.ObjectMeta.ResourceVersion != job.ObjectMeta.ResourceVersion {
				latestJob.Status = job.Status
				job = latestJob
			}
		}
		r.updateObjectStatus(job, oldJobStatus)
	}()

	if r.checkSuspended(job) {
		logger.Infof("job %s/%s receive timeout annotation", job.Namespace, job.Name)
		msg := fmt.Sprintf("TrainingJob %s is waiting resource timeout.", job.Name)
		updateJobConditions(job.GetJobStatus(), commonv1.Suspended, WaitingResourceTimeoutReason, msg)
		updatePhase(job.GetJobStatus(), commonv1.Suspended)
	}
	switch job.Status.Phase {
	case commonv1.JobSucceeded:
		err = r.cleanup(job)
	case commonv1.JobFailed:
		err = r.restartJob(job)
	case commonv1.Suspended:
		err = r.cleanupAll(job)
	case "", commonv1.JobCreated:
		r.initializeJob(job)
		err = r.reconcileResource(job)
	case commonv1.JobRunning:
		err = r.reconcileJobRunning(job)
	case commonv1.Scaling:
		err = r.executeScaling(job)
	default:
		logger.Warnf("job %s unknown status %s", job.Name, job.Status.Phase)
	}
	if err != nil {
		if IsRequeueError(err) {
			return RequeueAfterInterval(r.PollInterval, nil)
		}
		return RequeueAfterInterval(r.PollInterval, err)
	}
	return NoRequeue()
}

func (r *TrainingJobReconciler) checkSuspended(job *kaiv1alpha1.TrainingJob) bool {
	annotation := job.Annotations
	if status, ok := annotation[common.JobSuspended]; ok {
		if status == common.True {
			return true
		}
	}
	return false
}
func (r *TrainingJobReconciler) initializeJob(job *kaiv1alpha1.TrainingJob) {
	initializeStatuses(job.GetStatus().(*kaiv1alpha1.TrainingJobStatus), job.Name)
	if job.Status.Conditions == nil {
		initializeJobStatuses(job.GetJobStatus(), kaiv1alpha1.ETReplicaTypeLauncher)
		initializeJobStatuses(job.GetJobStatus(), kaiv1alpha1.ETReplicaTypeWorker)
		msg := fmt.Sprintf("TrainingJob %s is created.", job.Name)
		updateJobConditions(job.GetJobStatus(), commonv1.JobCreated, trainingJobCreatedReason, msg)
		updatePhase(job.GetJobStatus(), commonv1.JobCreated)
		logger.Infof(msg)
	}
	// first set StartTime.
	if job.Status.StartTime == nil {
		now := metav1.Now()
		job.Status.StartTime = &now
	}
	return
}

func (r *TrainingJobReconciler) resetJobStatus(job *kaiv1alpha1.TrainingJob) {
	// reset conditions for newSteps work
	job.Status.Conditions = []common.JobCondition{}
	job.Status.CurrentWorkers = []string{}
	job.Status.TargetWorkers = []string{}
	initializeJobStatuses(job.GetJobStatus(), kaiv1alpha1.ETReplicaTypeLauncher)
	initializeJobStatuses(job.GetJobStatus(), kaiv1alpha1.ETReplicaTypeWorker)
	initializeStatuses(job.GetStatus().(*kaiv1alpha1.TrainingJobStatus), job.Name)
	msg := fmt.Sprintf("TrainingJob %s is created.", job.Name)
	updateJobConditions(job.GetJobStatus(), commonv1.JobCreated, trainingJobCreatedReason, msg)
	updatePhase(job.GetJobStatus(), commonv1.JobCreated)
	logger.Infof(msg)
	return
}

func (r *TrainingJobReconciler) cleanup(job *kaiv1alpha1.TrainingJob) error {
	if job.Spec.CleanPodPolicy == nil {
		running := commonv1.CleanPodPolicyRunning
		job.Spec.CleanPodPolicy = &running
	}

	if isCleanUpPods(job.Spec.CleanPodPolicy) {
		if err := r.DeleteAllWorkerPods(job); err != nil {
			return err
		}
		if err := r.DeleteAllWorkerServices(job); err != nil {
			return err
		}
		logger.Infof("trainingjob(%v/%v) is %s, reconcile finished.", job.Namespace, job.Name, job.Status.Phase)
		return nil
	}

	return nil
}
func (r *TrainingJobReconciler) cleanupAll(job *kaiv1alpha1.TrainingJob) error {
	if err := r.DeleteAllWorkerPods(job); err != nil {
		logger.Infof("trainingjob(%v/%v) fail to DeleteAllWorkerPods", job.Namespace, job.Name)
		return err
	}
	if err := r.DeleteAllWorkerServices(job); err != nil {
		return err
	}

	if err := r.DeleteLauncher(job); err != nil {
		logger.Errorf("trainingjob(%v/%v) fail to Delete Launcher pod", job.Namespace, job.Name)
		return err
	}
	logger.Infof("trainingjob(%v/%v) is %s, reconcile finished.", job.Namespace, job.Name, job.Status.Phase)
	return nil

}

func (r *TrainingJobReconciler) needRestartJob(job *kaiv1alpha1.TrainingJob) bool {
	restartPolicy := job.Spec.RestartPolicy
	backoffLimit := *job.Spec.BackoffLimit
	restartCount := job.Status.RestartCount
	logger.Infof("trainingjob(%v/%v) check restart, current restartcount: %d", job.Namespace, job.Name, restartCount)
	if restartPolicy == commonv1.RestartPolicyOnFailure && restartCount < backoffLimit {
		job.Status.RestartCount = restartCount + 1
		logger.Infof("trainingjob(%v/%v) need to restart", job.Namespace, job.Name)
		return true
	}
	logger.Infof("trainingjob(%v/%v) no need to restart", job.Namespace, job.Name)
	return false
}
func (r *TrainingJobReconciler) restartJob(job *kaiv1alpha1.TrainingJob) error {
	if r.needRestartJob(job) {
		// reset job status

		r.resetJobStatus(job)
		// delete worker
		if err := r.DeleteAllWorkerPods(job); err != nil {
			logger.Errorf("trainingjob(%v/%v) fail to Delete All Workers", job.Namespace, job.Name)
			return err
		}
		// delete launcher
		if err := r.DeleteLauncher(job); err != nil {
			logger.Errorf("trainingjob(%v/%v) fail to Delete Launcher pod", job.Namespace, job.Name)
			return err
		}

		// delete service
		if err := r.DeleteAllWorkerServices(job); err != nil {
			logger.Errorf("trainingjob(%v/%v) fail to Delete All Service", job.Namespace, job.Name)
			return err
		}
		r.recorder.Eventf(job, corev1.EventTypeNormal, trainingJobSucceededReason, "Delete subresource for restart")

		// recreate
		if err := r.reconcileResource(job); err != nil {
			logger.Errorf("trainingjob(%v/%v) fail to reconcileResource", job.Namespace, job.Name)
			return err
		}
	} else {
		r.cleanup(job)
	}

	return nil
}

type Step struct {
	JobCondition commonv1.JobConditionType
	Action       func(job *kaiv1alpha1.TrainingJob) error
}

func (r *TrainingJobReconciler) reconcileResource(job *kaiv1alpha1.TrainingJob) error {
	steps := r.newSteps()
	err := r.doSteps(job, steps)
	if err != nil {
		r.Log.Error(err, "failed to reconcileResource")
	}
	return err
}

func (r *TrainingJobReconciler) newSteps() []Step {
	return []Step{
		Step{
			JobCondition: commonv1.WorkersCreated,
			Action:       r.createTrainingJobWorkers,
		},
		Step{
			JobCondition: commonv1.WorkersReady,
			Action:       r.waitWorkersRunning,
		},
		Step{
			JobCondition: commonv1.LauncherCreated,
			Action:       r.createLauncher,
		},
		Step{
			JobCondition: commonv1.JobRunning,
			Action:       r.syncLauncherState,
		},
	}
}

func (r *TrainingJobReconciler) doSteps(job *kaiv1alpha1.TrainingJob, steps []Step) error {
	for _, step := range steps {
		if hasCondition(*job.GetJobStatus(), step.JobCondition) {
			continue
		}
		err := step.Action(job)
		if err != nil {
			return err
		}
		break
	}
	return nil
}

func (r *TrainingJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	build := ctrl.NewControllerManagedBy(mgr).
		For(&kaiv1alpha1.TrainingJob{}).
		Owns(&kaiv1alpha1.ScaleIn{}).
		Owns(&kaiv1alpha1.ScaleOut{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{})

	if r.EnableCreateSecret {
		build = build.Owns(&corev1.Secret{})
	}

	return build.Complete(r)
}

// force overwrite RestartPolicy=Never
func setRestartPolicy(podTemplateSpec *corev1.PodTemplateSpec) {
	podTemplateSpec.Spec.RestartPolicy = corev1.RestartPolicyNever
}

func (r *TrainingJobReconciler) updateObjectStatus(obj RuntimeObject, oldStatus interface{}) error {
	return updateObjectStatus(r, obj, oldStatus)
}

func updateObjectStatus(c client.Client, obj RuntimeObject, oldStatus interface{}) error {
	// no need to update the job if the status hasn't changed since last time.
	if oldStatus != nil && reflect.DeepEqual(oldStatus, obj.GetStatus()) {
		// call apiserver of k8s to write job status
		return nil
	}
	err := c.Status().Update(context.Background(), obj.DeepCopyObject())
	if err != nil {
		logger.Warnf("update %s: %s status by apiserver failed, error: %v", obj.GetObjectKind().GroupVersionKind(), obj.GetName(), err)
		return err
	}
	return nil
}
