# ============================================================
# symkernel — Terraform outputs
# ============================================================
# Milestone 8 requires the module to expose the service endpoint
# and the monitoring dashboard URL for the deployed cloud. Each
# output resolves to the active cloud's value (empty string for
# clouds that were not selected) so consumers can read a single
# stable key regardless of var.cloud.
# ============================================================

# ------------------------------------------------------------
# Service endpoint — where clients send Criterion/ConstraintIR
# verification requests (the HTTP API symkerneld serves).
# ------------------------------------------------------------

output "service_endpoint" {
  description = "Public URL of the symkerneld service for the selected cloud. Callers POST Criterion/ConstraintIR verification requests here."
  value = {
    cloudflare = local.is_cloudflare ? "https://symkernel.workers.dev" : ""
    gcp        = local.is_gcp ? "https://${var.gke_cluster_name}.endpoints.${var.gcp_project_id}.cloud.goog" : ""
    aws        = local.is_aws ? aws_eks_cluster.symkernel[0].endpoint : ""
  }
}

# ------------------------------------------------------------
# Monitoring dashboard URL — Grafana / cloud-native ops console.
# ------------------------------------------------------------

output "monitoring_dashboard_url" {
  description = "Operations dashboard URL for the selected cloud (Cloudflare dashboard, GCP Cloud Monitoring, or AWS CloudWatch)."
  value = {
    cloudflare = local.is_cloudflare ? "https://dash.cloudflare.com/${var.cloudflare_account_id}/symkernel" : ""
    gcp        = local.is_gcp ? "https://console.cloud.google.com/monitoring/dashboards?project=${var.gcp_project_id}" : ""
    aws        = local.is_aws ? "https://${var.aws_region}.console.aws.amazon.com/cloudwatch/home?region=${var.aws_region}#dashboards:name=${var.eks_cluster_name}" : ""
  }
}

# ------------------------------------------------------------
# Selected cloud + cross-cutting deployment metadata.
# ------------------------------------------------------------

output "selected_cloud" {
  description = "The cloud this module is configured to deploy to."
  value       = var.cloud
}

output "container_image" {
  description = "The symkerneld container image deployed by this run."
  value       = var.container_image
}

output "service_port" {
  description = "The TCP port symkerneld listens on (SYMKERNEL_ADDR)."
  value       = var.service_port
}

# ------------------------------------------------------------
# Per-cloud resource handles (populated only for the active cloud).
# ------------------------------------------------------------

output "cloudflare_deployment_id" {
  description = "ID of the Cloudflare Container deployment, when var.cloud = cloudflare."
  value       = local.is_cloudflare ? cloudflare_container_deployment.symkernel[0].id : null
}

output "gke_cluster_name" {
  description = "Name of the GKE cluster, when var.cloud = gcp."
  value       = local.is_gcp ? google_container_cluster.symkernel[0].name : null
}

output "eks_cluster_name" {
  description = "Name of the EKS cluster, when var.cloud = aws."
  value       = local.is_aws ? aws_eks_cluster.symkernel[0].name : null
}

# ------------------------------------------------------------
# Optional Redis cache endpoint (only when enable_redis = true).
# ------------------------------------------------------------

output "redis_endpoint" {
  description = "Host of the managed Redis cache backend, when var.enable_redis = true. Consumed by internal/cache as the optional cross-instance cache."
  value = (
    local.is_gcp && var.enable_redis ? google_redis_instance.symkernel[0].host :
    local.is_aws && var.enable_redis ? aws_elasticache_cluster.symkernel[0].cache_nodes[0].address :
    null
  )
}
