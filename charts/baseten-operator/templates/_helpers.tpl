{{/*
Expand the name of the chart.
*/}}
{{- define "baseten-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create a fully qualified app name. Defaults to the release name so resources are named
`<release>-<component>` (e.g., `baseten-operator-controller-manager`) when installed with
`helm install baseten-operator ...`.
*/}}
{{- define "baseten-operator.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/*
Namespace the operator runs in.
*/}}
{{- define "baseten-operator.namespace" -}}
{{- default .Release.Namespace .Values.namespace.name -}}
{{- end -}}

{{/*
ServiceAccount name.
*/}}
{{- define "baseten-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.name -}}
{{- .Values.serviceAccount.name -}}
{{- else -}}
{{- printf "%s-controller-manager" (include "baseten-operator.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/*
Labels on kustomize-managed resources (ServiceAccount, leader-election Role/RoleBinding,
manager ClusterRoleBinding, aggregated user ClusterRoles).
*/}}
{{- define "baseten-operator.labels" -}}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/name: {{ include "baseten-operator.name" . }}
{{- end -}}

{{/*
Labels on the controller workload (Deployment, metrics Service).
*/}}
{{- define "baseten-operator.workloadLabels" -}}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/name: {{ include "baseten-operator.name" . }}
control-plane: controller-manager
{{- end -}}

{{/*
Selector labels for the controller Deployment and metrics Service.
*/}}
{{- define "baseten-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "baseten-operator.name" . }}
control-plane: controller-manager
{{- end -}}
