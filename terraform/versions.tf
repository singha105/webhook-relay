# Terraform, not OpenTofu, at the project owner's request.
#
# Terraform >= 1.6 is BUSL-licensed: free to use here, no account required, but
# not open source. Nothing in this directory depends on a HashiCorp-hosted
# service — no Terraform Cloud backend, no HCP, no registry beyond the public
# provider mirror — so switching to OpenTofu is `s/terraform/tofu/` in the
# Makefile and nothing else.

terraform {
  required_version = ">= 1.6.0"

  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.35"
    }
    helm = {
      source  = "hashicorp/helm"
      version = "~> 2.17"
    }
    kubectl = {
      # Used only for CRs that must be applied as raw YAML — CloudNativePG
      # Clusters and ArgoCD Applications. The kubernetes provider's
      # manifest resource needs the CRD to exist at PLAN time, which it does
      # not on a first apply, so it cannot express "install an operator and
      # then create its custom resource" in one run.
      source  = "gavinbunney/kubectl"
      version = "~> 1.19"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
    http = {
      source  = "hashicorp/http"
      version = "~> 3.4"
    }
  }

  # State is local, deliberately: this is a single-operator laptop cluster that
  # is created and destroyed on demand, and a remote backend would add an
  # account dependency the project forbids.
  #
  # In a team setting you would NOT do this. Two engineers running apply at the
  # same time against a local state file produce two divergent views of reality
  # and a cluster that matches neither. The fix is a backend with state locking:
  #
  #   terraform {
  #     backend "s3" {
  #       bucket         = "acme-tfstate"
  #       key            = "webhook-relay/cluster.tfstate"
  #       region         = "us-east-1"
  #       dynamodb_table = "terraform-locks"   # the lock, not the state
  #       encrypt        = true
  #     }
  #   }
  #
  # The DynamoDB table is the important half: S3 holds the state, but it is the
  # conditional write on the lock table that makes a second apply block instead
  # of racing. Any backend with a real lock works — GCS, Azure Blob with a
  # lease, or Postgres — the anti-pattern is a shared bucket with no lock at
  # all, which looks like it works right up until two people deploy on the same
  # afternoon.
  #
  # State also contains secrets in plaintext (generated passwords, certificates),
  # so the bucket needs encryption at rest, versioning for recovery, and an IAM
  # policy far narrower than the one used to run apply.
}

provider "kubernetes" {
  config_path    = var.kubeconfig_path
  config_context = var.kube_context
}

provider "helm" {
  kubernetes {
    config_path    = var.kubeconfig_path
    config_context = var.kube_context
  }
}

provider "kubectl" {
  config_path       = var.kubeconfig_path
  config_context    = var.kube_context
  load_config_file  = true
  apply_retry_count = 3
}
