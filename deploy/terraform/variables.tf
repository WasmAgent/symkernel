# ============================================================
# symkernel — Terraform input variables
# ============================================================
# Milestone 8 (docs/15-milestones.md): infrastructure as code for
# cloud deployment. This module provisions symkerneld on one of
# three target clouds (see var.cloud):
#
#   - cloudflare : Cloudflare Containers (the existing deployment
#                  target, mirroring deploy/wrangler.toml sizing)
#   - gcp        : GKE + CloudSQL, with optional Memorystore (Redis)
#   - aws        : EKS + ElastiCache (Redis)
#
# All variables have safe defaults so `terraform plan` works against
# a single selected cloud without requiring credentials for the
# others (resources are count-gated on var.cloud in main.tf).
# ============================================================

# ---- Cloud selection ---------------------------------------

variable "cloud" {
  description = "Target cloud for the symkerneld deployment. Selects which provider's resources are created."
  type        = string
  default     = "cloudflare"

  validation {
    condition     = contains(["cloudflare", "gcp", "aws"], var.cloud)
    error_message = "var.cloud must be one of: cloudflare, gcp, aws."
  }
}

variable "environment" {
  description = "Deployment environment label (e.g. dev, staging, prod). Used to name and tag resources."
  type        = string
  default     = "dev"
}

# ---- Image & service ---------------------------------------

variable "container_image" {
  description = "Fully-qualified container image for symkerneld (e.g. ghcr.io/wasmagent/symkernel:latest). On Cloudflare this may be the Dockerfile path handled by wrangler; on GKE/EKS it must be a registry-pushed image."
  type        = string
  default     = "ghcr.io/wasmagent/symkernel:latest"
}

variable "service_port" {
  description = "Port symkerneld listens on (SYMKERNEL_ADDR). Must match EXPOSE in the root Dockerfile."
  type        = number
  default     = 8080

  validation {
    condition     = var.service_port > 0 && var.service_port < 65536
    error_message = "var.service_port must be a valid TCP port (1-65535)."
  }
}

# ---- Autoscaling / sizing ----------------------------------

variable "min_replicas" {
  description = "Minimum number of symkerneld replicas/pods/instances. Single-instance deployments (Cloudflare Phase 0/1) ignore this."
  type        = number
  default     = 2
}

variable "max_replicas" {
  description = "Maximum number of symkerneld replicas for horizontal autoscaling. Set to 1 to disable scale-out."
  type        = number
  default     = 20
}

variable "instance_type" {
  description = "Compute shape. Cloudflare instance_type (e.g. standard-1), GCP machine_type (e.g. n2-standard-2), or AWS instance_type (e.g. t3.medium)."
  type        = string
  default     = "standard-1"
}

# ---- Cloudflare (existing target) --------------------------

variable "cloudflare_account_id" {
  description = "Cloudflare account ID. Set via TF_VAR_cloudflare_account_id or the CLOUDFLARE_ACCOUNT_ID env var; leave blank for non-cloudflare clouds."
  type        = string
  default     = ""
}

# ---- GCP (GKE + CloudSQL) ----------------------------------

variable "gcp_project_id" {
  description = "GCP project ID. Required when var.cloud = gcp."
  type        = string
  default     = ""
}

variable "gcp_region" {
  description = "GCP region for the cluster and managed services."
  type        = string
  default     = "us-central1"
}

variable "gke_cluster_name" {
  description = "Name of the GKE cluster to create (or adopt) for symkerneld."
  type        = string
  default     = "symkernel"
}

variable "cloudsql_tier" {
  description = "CloudSQL (PostgreSQL) machine tier for audit/metadata storage."
  type        = string
  default     = "db-f1-micro"
}

# ---- AWS (EKS + ElastiCache) -------------------------------

variable "aws_region" {
  description = "AWS region for the EKS cluster and managed services."
  type        = string
  default     = "us-east-1"
}

variable "eks_cluster_name" {
  description = "Name of the EKS cluster to create for symkerneld."
  type        = string
  default     = "symkernel"
}

# ---- Optional Redis cache ----------------------------------
# Mirrors the internal/cache optional Redis backend (M8 cache tier).
# On GCP this is Memorystore for Redis; on AWS, ElastiCache.

variable "enable_redis" {
  description = "Provision a managed Redis instance for the cross-instance cache backend (internal/cache). Disabled by default; the in-memory LRU is sufficient for single-instance deployments."
  type        = bool
  default     = false
}

variable "redis_node_type" {
  description = "Managed Redis node size. GCP tier (e.g. BASIC_HA-STANDARD-1) or AWS node type (e.g. cache.t3.micro)."
  type        = string
  default     = "cache.t3.micro"
}

# ---- Secrets -----------------------------------------------

variable "client_token_secret" {
  description = "Name of the secret (in the target cloud's secret manager) holding SYMKERNEL_CLIENT_TOKEN. The secret value is never read by Terraform; only its name/reference is wired into the deployment."
  type        = string
  default     = "symkernel-client-token"
}
