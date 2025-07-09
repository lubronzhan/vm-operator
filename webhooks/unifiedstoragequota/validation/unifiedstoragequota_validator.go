// © Broadcom. All Rights Reserved.
// The term “Broadcom” refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package validation

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	v1 "k8s.io/api/admission/v1"
	"k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmgr "sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/go-logr/logr"
	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha4"
	"github.com/vmware-tanzu/vm-operator/pkg/builder"
	pkgconst "github.com/vmware-tanzu/vm-operator/pkg/constants"
	pkgctx "github.com/vmware-tanzu/vm-operator/pkg/context"
	"github.com/vmware-tanzu/vm-operator/pkg/record"
)

const (
	webhookName = "vmservice.cns.vsphere.vmware.com"
	// Storage-policy-quota webhook infer the path by /getrequestedcapacityfor + resource kind
	// Related code:
	// https://github-vcf.devops.broadcom.net/vcf/cayman_photon/blob/01905fe326b5ee63549c2ed44a8623aa1fee8d78/controller-managers/storage-policy-quota/pkg/webhook/quota/quota_webhook.go#L446-L449
	vmQuotaWebhookPath         = "/getrequestedcapacityforvirtualmachine"
	vmSnapshotQuotaWebhookPath = "/getrequestedcapacityforvirtualmachinesnapshot"
	scParamStoragePolicyID     = "storagePolicyID"
)

var (
	emptyCapacity = resource.NewQuantity(0, resource.BinarySI)
)

// RequestedCapacity represents response body returned by this webhook to the SPQ webhook.
type RequestedCapacity struct {
	// Capacity represents the size to be reserved for the given resource
	Capacity resource.Quantity `json:"capacity"`

	// StorageClassName represents the StorageClass associated with the given resource
	StorageClassName string `json:"storageClassName"`

	// StoragePolicyID represents the StoragePolicyId associated with the given resource
	StoragePolicyID string `json:"storagePolicyId"`

	// Reason indicates the cause for returning capacity as 0
	Reason string `json:"reason,omitempty"`
}

type CapacityResponse struct {
	RequestedCapacity
	admission.Response
}

type CapacityHandler interface {
	Handle(req admission.Request) CapacityResponse
	WriteResponse(w http.ResponseWriter, resp CapacityResponse)
}
type VMRequestedCapacityHandler struct {
	*pkgctx.WebhookContext
	admission.Decoder

	Client    client.Client
	Converter runtime.UnstructuredConverter
}

type VMSnapshotRequestedCapacityHandler struct {
	*pkgctx.WebhookContext
	admission.Decoder

	Client    client.Client
	Converter runtime.UnstructuredConverter
}

type storageClassCapacity struct {
	capacity        *resource.Quantity
	scName          string
	storagePolicyID string
}

// AddToManager adds the webhook to the provided manager.
func AddToManager(ctx *pkgctx.ControllerManagerContext, mgr ctrlmgr.Manager) error {
	webhookNameLong := fmt.Sprintf("%s/%s/%s", ctx.Namespace, ctx.Name, webhookName)

	// Build the webhookContext.
	webhookContext := &pkgctx.WebhookContext{
		Context:            ctx,
		Name:               webhookName,
		Namespace:          ctx.Namespace,
		ServiceAccountName: ctx.ServiceAccountName,
		Recorder:           record.New(mgr.GetEventRecorderFor(webhookNameLong)),
		Logger:             ctx.Logger.WithName(webhookName),
	}
	// Initialize the webhook's decoder.
	decoder := admission.NewDecoder(mgr.GetScheme())

	vmQuotaHandler := &VMRequestedCapacityHandler{
		Client:         mgr.GetClient(),
		Converter:      runtime.DefaultUnstructuredConverter,
		Decoder:        decoder,
		WebhookContext: webhookContext,
	}

	vmSnapshotQuotaHandler := &VMSnapshotRequestedCapacityHandler{
		Client:         mgr.GetClient(),
		Converter:      runtime.DefaultUnstructuredConverter,
		Decoder:        decoder,
		WebhookContext: webhookContext,
	}

	mgr.GetWebhookServer().Register(vmQuotaWebhookPath, vmQuotaHandler)
	mgr.GetWebhookServer().Register(vmSnapshotQuotaWebhookPath, vmSnapshotQuotaHandler)

	return nil
}

/*************************** Handler for VM requested capacity ***************************/

func (h *VMRequestedCapacityHandler) Handle(req admission.Request) CapacityResponse {
	var (
		obj, oldObj   *unstructured.Unstructured
		handleRequest func(ctx *pkgctx.WebhookRequestContext) CapacityResponse
	)

	if req.Operation == v1.Create || req.Operation == v1.Update {
		obj = &unstructured.Unstructured{}
		if err := h.DecodeRaw(req.Object, obj); err != nil {
			return CapacityResponse{Response: webhook.Errored(http.StatusBadRequest, err)}
		}
	}

	if _, ok := obj.GetAnnotations()[pkgconst.SkipValidationAnnotationKey]; ok {
		// The VM has the skip validation annotation, so just allow this VM to
		// effectively bypass quota validation by returning 0 to the quota
		// framework.
		return CapacityResponse{Response: webhook.Allowed(builder.SkipValidationAllowed)}
	}

	switch req.Operation {
	case v1.Create:
		handleRequest = h.HandleCreate
	case v1.Update:
		oldObj = &unstructured.Unstructured{}
		if err := h.DecodeRaw(req.OldObject, oldObj); err != nil {
			return CapacityResponse{Response: webhook.Errored(http.StatusBadRequest, err)}
		}
		handleRequest = h.HandleUpdate
	default:
		return CapacityResponse{Response: webhook.Allowed(string(req.Operation))}
	}

	if obj == nil {
		return CapacityResponse{Response: webhook.Allowed(string(req.Operation))}
	}

	webhookRequestContext := &pkgctx.WebhookRequestContext{
		WebhookContext: h.WebhookContext,
		Op:             req.Operation,
		Obj:            obj,
		OldObj:         oldObj,
		UserInfo:       req.UserInfo,
		Logger:         h.WebhookContext.Logger.WithName(obj.GetNamespace()).WithName(obj.GetName()),
	}

	return handleRequest(webhookRequestContext)
}

// HandleCreate returns the Boot Disk capacity from the corresponding VMI/CVMI for the VM object in the AdmissionRequest.
func (h *VMRequestedCapacityHandler) HandleCreate(ctx *pkgctx.WebhookRequestContext) CapacityResponse {
	vm := &vmopv1.VirtualMachine{}
	if err := h.Converter.FromUnstructured(ctx.Obj.UnstructuredContent(), vm); err != nil {
		return CapacityResponse{Response: webhook.Errored(http.StatusBadRequest, err)}
	}

	scName := vm.Spec.StorageClass

	sc := &storagev1.StorageClass{}
	if err := h.Client.Get(ctx, client.ObjectKey{Name: scName}, sc); err != nil {
		if apierrors.IsNotFound(err) {
			return CapacityResponse{Response: webhook.Errored(http.StatusNotFound, err)}
		}
		return CapacityResponse{Response: webhook.Errored(http.StatusInternalServerError, err)}
	}

	// This webhook will not be called if vm.Spec.Image is not set. This is done in order to ensure that imageless VMs
	// do not participate in quota validation. The VirtualMachine ValidatingWebhook still performs image validation.
	//
	// Please see https://github.com/vmware-tanzu/vm-operator/pull/822 and
	// https://github.com/vmware-tanzu/vm-operator/blob/main/config/webhook/storage_quota_webhook_configuration.yaml#L30-L31
	vmiName := vm.Spec.Image.Name
	var imageStatus vmopv1.VirtualMachineImageStatus

	switch vm.Spec.Image.Kind {
	case "VirtualMachineImage":
		vmi := &vmopv1.VirtualMachineImage{}
		if err := h.Client.Get(ctx, client.ObjectKey{Namespace: vm.Namespace, Name: vmiName}, vmi); err != nil {
			if apierrors.IsNotFound(err) {
				return CapacityResponse{Response: webhook.Errored(http.StatusNotFound, err)}
			}
			return CapacityResponse{Response: webhook.Errored(http.StatusInternalServerError, err)}
		}
		imageStatus = vmi.Status
	case "ClusterVirtualMachineImage":
		cvmi := &vmopv1.ClusterVirtualMachineImage{}
		if err := h.Client.Get(ctx, client.ObjectKey{Name: vmiName}, cvmi); err != nil {
			if apierrors.IsNotFound(err) {
				return CapacityResponse{Response: webhook.Errored(http.StatusNotFound, err)}
			}
			return CapacityResponse{Response: webhook.Errored(http.StatusInternalServerError, err)}
		}
		imageStatus = cvmi.Status
	default:
		return CapacityResponse{Response: webhook.Errored(http.StatusBadRequest, fmt.Errorf("unsupported image kind %s", vm.Spec.Image.Kind))}
	}

	// Skip ISO images as they don't have boot disks.
	if imageStatus.Type == "ISO" {
		return CapacityResponse{Response: webhook.Allowed("")}
	}

	if len(imageStatus.Disks) < 1 {
		return CapacityResponse{Response: webhook.Errored(http.StatusNotFound, errors.New("no disks found in image status"))}
	}

	capacity := resource.NewQuantity(0, resource.BinarySI)
	// This is used to add zero to the total capacity for any disks in the image status that have nil capacity.
	emptyCapacity := resource.NewQuantity(0, resource.BinarySI)

	for _, disk := range imageStatus.Disks {
		if disk.Capacity == nil {
			capacity.Add(*emptyCapacity)
		} else {
			capacity.Add(*disk.Capacity)
		}
	}

	return CapacityResponse{
		RequestedCapacity: RequestedCapacity{
			Capacity:         *capacity,
			StorageClassName: scName,
			// If this parameter does not exist, then it is not necessarily an error condition. Return
			// an empty value for StoragePolicyID and let Storage Policy Quota extension service decide
			// what to do.
			StoragePolicyID: sc.Parameters[scParamStoragePolicyID],
		},
		Response: webhook.Allowed(""),
	}
}

// HandleUpdate checks for any positive difference in boot disk size and returns that difference.
//   - If both vm and oldVM have Spec.Advanced.BootDiskCapacity set, then only return a positive difference.
//   - If vm has Spec.Advanced.BootDiskCapacity set, while it is not set for oldVM, then use the first classic disk in
//     oldVM.Status.Volumes as this basis for comparison, again returning only a positive difference.
//   - If vm does not have Spec.Advanced.BootDiskCapacity set, then return an empty response.
func (h *VMRequestedCapacityHandler) HandleUpdate(ctx *pkgctx.WebhookRequestContext) CapacityResponse {
	if !ctx.Obj.GetDeletionTimestamp().IsZero() {
		return CapacityResponse{Response: admission.Allowed(builder.AdmitMesgUpdateOnDeleting)}
	}
	vm := &vmopv1.VirtualMachine{}
	if err := h.Converter.FromUnstructured(ctx.Obj.UnstructuredContent(), vm); err != nil {
		return CapacityResponse{Response: webhook.Errored(http.StatusBadRequest, err)}
	}

	oldVM := &vmopv1.VirtualMachine{}
	if err := h.Converter.FromUnstructured(ctx.OldObj.UnstructuredContent(), oldVM); err != nil {
		return CapacityResponse{Response: webhook.Errored(http.StatusBadRequest, err)}
	}

	var capacity, oldCapacity *resource.Quantity

	if vm.Spec.Advanced == nil || vm.Spec.Advanced.BootDiskCapacity == nil {
		return CapacityResponse{Response: webhook.Allowed("")}
	}

	capacity = vm.Spec.Advanced.BootDiskCapacity

	if oldVM.Spec.Advanced == nil || oldVM.Spec.Advanced.BootDiskCapacity == nil {
		oldCapacity = resource.NewQuantity(0, resource.BinarySI)
		for _, volume := range oldVM.Status.Volumes {
			if volume.Type == vmopv1.VirtualMachineStorageDiskTypeClassic {
				if volume.Limit != nil {
					oldCapacity = volume.Limit
					break
				}
			}
		}
	} else {
		oldCapacity = oldVM.Spec.Advanced.BootDiskCapacity
	}

	if capacity.Cmp(*oldCapacity) != 1 {
		return CapacityResponse{Response: webhook.Allowed("")}
	}
	capacity.Sub(*oldCapacity)

	scName := vm.Spec.StorageClass
	sc := &storagev1.StorageClass{}
	if err := h.Client.Get(ctx, client.ObjectKey{Name: scName}, sc); err != nil {
		if apierrors.IsNotFound(err) {
			return CapacityResponse{Response: webhook.Errored(http.StatusNotFound, err)}
		}
		return CapacityResponse{Response: webhook.Errored(http.StatusInternalServerError, err)}
	}

	return CapacityResponse{
		RequestedCapacity: RequestedCapacity{
			Capacity:         *capacity,
			StorageClassName: scName,
			// If this parameter does not exist, then it is not necessarily an error condition. Return
			// an empty value for StoragePolicyID and let Storage Policy Quota extension service decide
			// what to do.
			StoragePolicyID: sc.Parameters[scParamStoragePolicyID],
		},
		Response: webhook.Allowed(""),
	}
}

var admissionScheme = runtime.NewScheme()
var admissionCodecs = serializer.NewCodecFactory(admissionScheme)

const maxRequestSize = int64(7 * 1024 * 1024)

func init() {
	utilruntime.Must(v1.AddToScheme(admissionScheme))
	utilruntime.Must(v1beta1.AddToScheme(admissionScheme))
}

func (h *VMRequestedCapacityHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	serverHttp(w, r, h, h.WebhookContext.Logger)
}

func (h *VMRequestedCapacityHandler) GetWebhookContext() *pkgctx.WebhookContext {
	return h.WebhookContext
}

func (h *VMRequestedCapacityHandler) WriteResponse(w http.ResponseWriter, response CapacityResponse) {
	writeResponse(w, response, h.WebhookContext.Logger)
}

/*************************** Handler for VM Snapshot requested capacity ***************************/

// Handle returns the requested capacity for the VMSnapshot object in the AdmissionRequest.
func (h *VMSnapshotRequestedCapacityHandler) Handle(req admission.Request) []*CapacityResponse {
	if req.Operation != v1.Create {
		return []*CapacityResponse{&CapacityResponse{Response: webhook.Allowed(string(req.Operation))}}
	}

	obj := &unstructured.Unstructured{}
	if err := h.DecodeRaw(req.Object, obj); err != nil {
		return []*CapacityResponse{&CapacityResponse{Response: webhook.Errored(http.StatusBadRequest, err)}}
	}

	if _, ok := obj.GetAnnotations()[pkgconst.SkipValidationAnnotationKey]; ok {
		// The VM Snapshot has the skip validation annotation, so just allow this VM to
		// effectively bypass quota validation by returning 0 to the quota
		// framework.
		return []*CapacityResponse{&CapacityResponse{Response: webhook.Allowed(builder.SkipValidationAllowed)}}
	}

	webhookRequestContext := &pkgctx.WebhookRequestContext{
		WebhookContext: h.WebhookContext,
		Op:             req.Operation,
		Obj:            obj,
		UserInfo:       req.UserInfo,
		Logger:         h.WebhookContext.Logger.WithName(obj.GetNamespace()).WithName(obj.GetName()),
	}

	return []*CapacityResponse{h.HandleCreate(webhookRequestContext)}
	// TODO: Blocked by https://vmw-jira.broadcom.net/browse/VMLM-6350 to return
	// CapacityResponse for multiple storage classes.
	// Now just return the capacity for the VM.
}

// HandleCreate returns the total capacity for the VMSnapshot object in the AdmissionRequest.
// It includes capacity of disks and PVCs included in the VM that referenced by the VMSnapshot object.
func (h *VMSnapshotRequestedCapacityHandler) HandleCreate(ctx *pkgctx.WebhookRequestContext) *CapacityResponse {
	vmSnapshot := &vmopv1.VirtualMachineSnapshot{}
	if err := h.Converter.FromUnstructured(ctx.Obj.UnstructuredContent(), vmSnapshot); err != nil {
		return &CapacityResponse{Response: webhook.Errored(http.StatusBadRequest, err)}
	}

	if vmSnapshot.Spec.VMRef == nil {
		return &CapacityResponse{Response: webhook.Errored(http.StatusBadRequest, errors.New("vmRef is not set"))}
	}

	vmName := vmSnapshot.Spec.VMRef.Name

	vm := &vmopv1.VirtualMachine{}
	if err := h.Client.Get(ctx, client.ObjectKey{Name: vmName}, vm); err != nil {
		if apierrors.IsNotFound(err) {
			return &CapacityResponse{Response: webhook.Errored(http.StatusNotFound, err)}
		}
		return &CapacityResponse{Response: webhook.Errored(http.StatusInternalServerError, err)}
	}

	vmSCName := vm.Spec.StorageClass
	sc := &storagev1.StorageClass{}
	if err := h.Client.Get(ctx, client.ObjectKey{Name: vmSCName}, sc); err != nil {
		if apierrors.IsNotFound(err) {
			return &CapacityResponse{Response: webhook.Errored(http.StatusNotFound, err)}
		}
		return &CapacityResponse{Response: webhook.Errored(http.StatusInternalServerError, err)}
	}

	// store the mapping of each storage class name to its capacity response
	capacityResponseMap := make(map[string]storageClassCapacity)
	capacityResponseMap[vmSCName] = newStorageClassCapacity(vmSCName, sc)

	// Calculate the total capacity of the disks and PVCs included in the VM.
	for _, volume := range vm.Status.Volumes {
		switch volume.Type {
		case vmopv1.VirtualMachineStorageDiskTypeClassic:
			if volume.Limit == nil {
				return &CapacityResponse{Response: webhook.Errored(http.StatusBadRequest, errors.New("limit is not set for classic disk"))}
			}
			capacityResponseMap[vmSCName].capacity.Add(*volume.Limit)
		case vmopv1.VirtualMachineStorageDiskTypeManaged:
			pvc := &corev1.PersistentVolumeClaim{}
			if err := h.Client.Get(ctx, client.ObjectKey{Namespace: ctx.Obj.GetNamespace(), Name: volume.Name}, pvc); err != nil {
				if apierrors.IsNotFound(err) {
					return &CapacityResponse{Response: webhook.Errored(http.StatusNotFound, err)}
				}
				return &CapacityResponse{Response: webhook.Errored(http.StatusInternalServerError, err)}
			}

			limit := volume.Limit
			if limit == nil {
				// If the limit is not set on volume, then use the PVC's limit
				if pvc.Spec.Resources.Limits == nil {
					return &CapacityResponse{Response: webhook.Errored(http.StatusInternalServerError, errors.New("limit is not set for managed disk"))}
				}
				limit = pvc.Spec.Resources.Limits.Storage()
			}

			// Fetch the storage class for the PVC
			if pvc.Spec.StorageClassName == nil {
				return &CapacityResponse{Response: webhook.Errored(http.StatusInternalServerError, errors.New("storageClassName is not set for managed disk"))}
			}

			scName := *pvc.Spec.StorageClassName
			if _, ok := capacityResponseMap[scName]; !ok {
				sc := &storagev1.StorageClass{}
				if err := h.Client.Get(ctx, client.ObjectKey{Name: scName}, sc); err != nil {
					if apierrors.IsNotFound(err) {
						return &CapacityResponse{Response: webhook.Errored(http.StatusNotFound, err)}
					}
					return &CapacityResponse{Response: webhook.Errored(http.StatusInternalServerError, err)}
				}
				capacityResponseMap[scName] = newStorageClassCapacity(scName, sc)
			}
			capacityResponseMap[scName].capacity.Add(*limit)
		default:
			return &CapacityResponse{Response: webhook.Errored(http.StatusBadRequest, fmt.Errorf("unsupported volume type %s", volume.Type))}
		}
	}

	// TODO: Blocked by https://vmw-jira.broadcom.net/browse/VMLM-6350 to return
	// CapacityResponse for multiple storage classes.
	// Now just return the capacity for the VM.
	return &CapacityResponse{
		RequestedCapacity: RequestedCapacity{
			Capacity:         *capacityResponseMap[vmSCName].capacity,
			StorageClassName: vmSCName,
			// If this parameter does not exist, then it is not necessarily an error condition. Return
			// an empty value for StoragePolicyID and let Storage Policy Quota extension service decide
			// what to do.
			StoragePolicyID: sc.Parameters[scParamStoragePolicyID],
		},
		Response: webhook.Allowed(""),
	}
}

func (h *VMSnapshotRequestedCapacityHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	serverHttp(w, r, h, h.WebhookContext.Logger)
}

func (h *VMSnapshotRequestedCapacityHandler) WriteResponse(w http.ResponseWriter, response CapacityResponse) {
	writeResponse(w, response, h.WebhookContext.Logger)
}

func serverHttp(w http.ResponseWriter, r *http.Request, h CapacityHandler, logger logr.Logger) {
	if r.Body == nil || r.Body == http.NoBody {
		err := errors.New("request body is empty")
		logger.Error(err, "bad request")
		h.WriteResponse(w, CapacityResponse{Response: webhook.Errored(http.StatusBadRequest, err)})
		return
	}

	defer func(Body io.ReadCloser) {
		_ = Body.Close()
	}(r.Body)

	limitedReader := &io.LimitedReader{R: r.Body, N: maxRequestSize}
	body, err := io.ReadAll(limitedReader)
	if err != nil {
		logger.Error(err, "unable to read the body from the incoming request")
		h.WriteResponse(w, CapacityResponse{Response: webhook.Errored(http.StatusBadRequest, err)})
		return
	}
	if limitedReader.N <= 0 {
		err := fmt.Errorf("request entity is too large; limit is %d bytes", maxRequestSize)
		logger.Error(err, "unable to read the body from the incoming request; limit reached")
		h.WriteResponse(w, CapacityResponse{Response: webhook.Errored(http.StatusRequestEntityTooLarge, err)})
		return
	}

	// verify the content type is accurate
	if contentType := r.Header.Get("Content-Type"); contentType != "application/json" {
		err = fmt.Errorf("contentType=%s, expected application/json", contentType)
		logger.Error(err, "unable to process a request with unknown content type")
		h.WriteResponse(w, CapacityResponse{Response: webhook.Errored(http.StatusBadRequest, err)})
		return
	}

	req := admission.Request{}
	ar := unversionedAdmissionReview{}
	// avoid an extra copy
	ar.Request = &req.AdmissionRequest
	ar.SetGroupVersionKind(v1.SchemeGroupVersion.WithKind("AdmissionReview"))
	_, _, err = admissionCodecs.UniversalDeserializer().Decode(body, nil, &ar)
	if err != nil {
		logger.Error(err, "unable to decode the request")
		h.WriteResponse(w, CapacityResponse{Response: webhook.Errored(http.StatusBadRequest, err)})
		return
	}
	logger.V(5).Info("received request")

	h.WriteResponse(w, h.Handle(req))
}

func writeResponse(w http.ResponseWriter, response CapacityResponse, logger logr.Logger) {
	if !response.Response.Allowed {
		response.Reason = response.Response.Result.Message
		w.WriteHeader(int(response.Response.Result.Code))
	}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		logger.Error(err, "unable to encode and write the response")

		serverError := webhook.Errored(http.StatusInternalServerError, err)
		if err = json.NewEncoder(w).Encode(v1.AdmissionReview{Response: &serverError.AdmissionResponse}); err != nil {
			logger.Error(err, "still unable to encode and write the InternalServerError response")
		}
	}
}

func newStorageClassCapacity(scName string, sc *storagev1.StorageClass) storageClassCapacity {
	return storageClassCapacity{
		capacity:        emptyCapacity,
		scName:          scName,
		storagePolicyID: sc.Parameters[scParamStoragePolicyID],
	}
}

// unversionedAdmissionReview is used to decode both v1 and v1beta1 AdmissionReview types.
type unversionedAdmissionReview struct {
	v1.AdmissionReview
}

var _ runtime.Object = &unversionedAdmissionReview{}
