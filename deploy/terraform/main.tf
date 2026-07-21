# ============================================================
# symkernel — Terraform root module (Milestone 8)
# ============================================================
# Infrastructure as code for the symkerneld verification service
# across three deployment targets (docs/15-milestones.md M8):
#
#   1. Cloudflare Containers  (var.cloud = "cloudflare") — the
#      existing target; mirrors deploy/wrangler.toml sizing.
#   2. GKE + CloudSQL         (var.cloud = "gcp")
#   3. EKS + ElastiCache      (var.cloud = "aws")
#
# Only one cloud's resources are created at a time: every resource
# block is count-gated on var.cloud, and providers are declared
# without required credentials for the non-selected clouds.
#
# Provider versions are pinned for reproducibility. To apply a
# specific cloud, export that provider's credentials and run:
#
#   terraform init
#   terraform plan  -var cloud=gcp -var gcp_project_id=...
#   terraform apply -var cloud=gcp -var gcp_project_id=...
# ============================================================

terraform {
  required_version = ">= 1.5.0"

  required_providers {
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "~> 4.0"
    }
    google = {
      source  = "hashicorp/google"
      version = "~> 5.0"
    }
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

# ------------------------------------------------------------
# Providers
# ------------------------------------------------------------
# `alias`-free root providers; each only needs credentials when the
# matching cloud is selected. Credentials are supplied via the
# standard provider env vars (CLOUDFLARE_API_TOKEN, GOOGLE_APPLICATION_CREDENTIALS,
# AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY).

provider "cloudflare" {}

provider "google" {
  project = var.gcp_project_id
  region  = var.gcp_region
}

provider "aws" {
  region = var.aws_region
}

# ============================================================
# 1. Cloudflare Containers (existing target)
# ============================================================
# Mirrors deploy/wrangler.toml: a single standard-1 Container behind
# a Worker. The cloudflare_container_deployment resource drives the
# same API that `wrangler deploy` targets, so this is the IaC path
# for the existing deployment rather than a second target.

locals {
  is_cloudflare = var.cloud == "cloudflare"
}

resource "cloudflare_container_deployment" "symkernel" {
  count      = local.is_cloudflare ? 1 : 0
  account_id = var.cloudflare_account_id

  # The container runs symkerneld, serving /v1/verify/cel,
  # /v1/verify/wasm, /v1/verify/z3, and the composed/batch tiers.
  image = var.container_image

  name = "symkernel-${var.environment}"

  # Phase 0/1 sizing matches deploy/wrangler.toml: one standard-1
  # instance (1/2 vCPU, 4 GiB RAM). Bump max_instances if the M3
  # decision gate (p99 > 2s or cold-start > 5s) trips.
  instance_type = var.instance_type
  max_instances = local.is_cloudflare ? max(var.max_replicas, 1) : 1

  env = {
    SYMKERNEL_ADDR        = ":${var.service_port}"
    SYMKERNEL_CLIENT_TOKEN = var.client_token_secret
  }
}

# ============================================================
# 2. GCP — GKE + CloudSQL (+ optional Memorystore Redis)
# ============================================================

locals {
  is_gcp = var.cloud == "gcp"
}

# --- GKE control plane --------------------------------------

resource "google_container_cluster" "symkernel" {
  count      = local.is_gcp ? 1 : 0
  name       = var.gke_cluster_name
  project    = var.gcp_project_id
  location   = var.gcp_region
  network    = "default"
  subnetwork = "default"

  # Remove the default node pool so the node_group below fully owns sizing.
  initial_node_count = 1
  remove_default_node_pool = true
}

# --- GKE node pool (autoscaled, 2..max_replicas) ------------

resource "google_container_node_pool" "symkernel" {
  count      = local.is_gcp ? 1 : 0
  name       = "${var.gke_cluster_name}-nodes"
  project    = var.gcp_project_id
  cluster    = google_container_cluster.symkernel[0].name
  location   = var.gcp_region

  initial_node_count = 1

  autoscaling {
    min_node_count = 1
    max_node_count = max(var.max_replicas, var.min_replicas)
  }

  node_config {
    machine_type = var.instance_type
    # oauth_scopes for the GCE default service account (logging/monitoring + CloudSQL).
    oauth_scopes = [
      "https://www.googleapis.com/auth/cloud-platform",
    ]
  }
}

# --- CloudSQL (PostgreSQL) for audit/metadata ---------------

resource "google_sql_database_instance" "symkernel" {
  count            = local.is_gcp ? 1 : 0
  name             = "${var.gke_cluster_name}-${var.environment}"
  project          = var.gcp_project_id
  region           = var.gcp_region
  database_version = "POSTGRES_15"

  settings {
    tier = var.cloudsql_tier
  }

  deletion_protection = false
}

# --- Optional Memorystore (Redis) cache backend -------------

resource "google_redis_instance" "symkernel" {
  count          = local.is_gcp && var.enable_redis ? 1 : 0
  name           = "${var.gke_cluster_name}-${var.environment}-cache"
  project        = var.gcp_project_id
  region         = var.gcp_region
  tier           = "BASIC"
  memory_size_gb = 1
  redis_version  = "REDIS_7_0"
}

# ============================================================
# 3. AWS — EKS + ElastiCache (Redis)
# ============================================================

locals {
  is_aws = var.cloud == "aws"
}

# --- EKS cluster IAM role -----------------------------------

data "aws_iam_policy_document" "eks_assume" {
  count = local.is_aws ? 1 : 0
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["eks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "eks_cluster" {
  count              = local.is_aws ? 1 : 0
  name               = "${var.eks_cluster_name}-cluster-role"
  assume_role_policy = data.aws_iam_policy_document.eks_assume[0].json
}

resource "aws_iam_role_policy_attachment" "eks_cluster" {
  count      = local.is_aws ? 1 : 0
  role       = aws_iam_role.eks_cluster[0].name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKSClusterPolicy"
}

# --- EKS control plane --------------------------------------

resource "aws_eks_cluster" "symkernel" {
  count    = local.is_aws ? 1 : 0
  name     = var.eks_cluster_name
  role_arn = aws_iam_role.eks_cluster[0].arn

  vpc_config {
    # Defaults; supply subnet_ids via vars when targeting a real VPC.
    endpoint_public_access = true
  }
}

# --- EKS node group (autoscaled) ----------------------------

resource "aws_eks_node_group" "symkernel" {
  count           = local.is_aws ? 1 : 0
  cluster_name    = aws_eks_cluster.symkernel[0].name
  node_group_name = "${var.eks_cluster_name}-nodes"
  node_role_arn   = aws_iam_role.eks_cluster[0].arn

  # subnet_ids supplied via vars in a real account.
  scaling_config {
    desired_size = var.min_replicas
    min_size     = var.min_replicas
    max_size     = max(var.max_replicas, var.min_replicas)
  }

  instance_types = [var.instance_type]
}

# --- Optional ElastiCache (Redis) cache backend -------------

resource "aws_elasticache_cluster" "symkernel" {
  count                = local.is_aws && var.enable_redis ? 1 : 0
  cluster_id           = "${var.eks_cluster_name}-${var.environment}-cache"
  engine               = "redis"
  node_type            = var.redis_node_type
  num_cache_nodes      = 1
  parameter_group_name = "default.redis7"
  apply_immediately    = true
}
