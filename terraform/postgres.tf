# CloudNativePG: the operator, then a Cluster custom resource.
#
# Why an operator rather than a StatefulSet with a Postgres image: failover.
# A StatefulSet restarts a pod; it has no idea which instance is the primary,
# cannot promote a replica, and will happily point the service at a pod that is
# still replaying WAL. CloudNativePG runs a real replication topology, watches
# for the primary going away, promotes a standby, and repoints the read-write
# service — which is precisely the behaviour the Day 4 runbook and Day 5's
# chaos testing exercise.

resource "helm_release" "cloudnative_pg" {
  name       = "cloudnative-pg"
  repository = "https://cloudnative-pg.github.io/charts"
  chart      = "cloudnative-pg"
  version    = var.chart_versions.cloudnative_pg
  namespace  = kubernetes_namespace.this["webhook-relay"].metadata[0].name

  # The operator installs CRDs. Nothing that creates a Cluster resource can be
  # planned until they exist, which is why the Cluster below is a kubectl
  # manifest with an explicit depends_on rather than a kubernetes_manifest.
  atomic          = true
  cleanup_on_fail = true
  # Raised from 600 on Day 6. On a laptop with Docker capped at ~5 GiB, image
  # pulls and scheduling on a cold cluster routinely exceed the old value, and
  # atomic = true then UNINSTALLS a release whose pods were already Running --
  # so bootstrap failed while the cluster was in the middle of coming up
  # correctly. A generous timeout costs nothing when things are fast.
  timeout = 1800
  values = [yamlencode({
    resources = {
      requests = { cpu = "50m", memory = "100Mi" }
      # The operator is a control loop, not a data path: it reconciles a
      # handful of objects and is idle the rest of the time. A generous limit
      # would just reserve memory nothing uses.
      limits = { memory = "200Mi" }
    }
    config = {
      data = {
        # Without this the operator logs at info for every reconcile of every
        # object, which on a small cluster is most of the log volume.
        INHERITED_ANNOTATIONS = "categories"
      }
    }
  })]
}

# The application database password.
#
# Generated rather than committed. CloudNativePG will create its own secret if
# none exists, but then the value is unknowable to anything outside the cluster
# — including the migration job's connection string and a developer running
# psql. Generating it here means Terraform knows it, can hand it to the app's
# secret, and it never appears in the repository.
resource "random_password" "postgres_app" {
  length  = 32
  special = false # keeps the value safe to embed in a URL without escaping
}

resource "kubernetes_secret" "postgres_app_credentials" {
  metadata {
    name      = "webhook-relay-db-credentials"
    namespace = kubernetes_namespace.this["webhook-relay"].metadata[0].name
    labels    = local.common_labels
  }

  # CloudNativePG expects this exact type and these exact keys for a bootstrap
  # secret; anything else and it generates its own and ignores this.
  type = "kubernetes.io/basic-auth"

  data = {
    username = var.postgres_owner
    password = random_password.postgres_app.result
  }
}

resource "kubectl_manifest" "postgres_cluster" {
  depends_on = [
    helm_release.cloudnative_pg,
    kubernetes_secret.postgres_app_credentials,
  ]

  yaml_body = yamlencode({
    apiVersion = "postgresql.cnpg.io/v1"
    kind       = "Cluster"
    metadata = {
      name      = "webhook-relay-db"
      namespace = kubernetes_namespace.this["webhook-relay"].metadata[0].name
      labels    = local.common_labels
    }
    spec = {
      instances = local.effective.postgres_instances

      imageName = "ghcr.io/cloudnative-pg/postgresql:16.6"

      # Instances land on different nodes where possible. On a single-node
      # cluster this is unsatisfiable, so it is a preference rather than a
      # requirement — a hard anti-affinity would leave two of three replicas
      # permanently Pending on the lowmem profile.
      affinity = {
        enablePodAntiAffinity = true
        topologyKey           = "kubernetes.io/hostname"
        podAntiAffinityType   = "preferred"
      }

      bootstrap = {
        initdb = {
          database = var.postgres_database
          owner    = var.postgres_owner
          secret   = { name = kubernetes_secret.postgres_app_credentials.metadata[0].name }
        }
      }

      storage = {
        size = var.postgres_storage_size
      }

      postgresql = {
        parameters = {
          # Tuned for a laptop, not for throughput. The defaults assume a
          # machine with far more memory than a k3d node has, and an
          # over-provisioned shared_buffers is the fastest way to get a
          # Postgres pod OOM-killed.
          max_connections      = "100"
          shared_buffers       = "128MB"
          effective_cache_size = "256MB"
          work_mem             = "4MB"
          # The application uses UUIDv7 primary keys, so inserts append at the
          # right edge and never need a random-page fetch. Telling the planner
          # the storage is not spinning rust matters more than usual here.
          random_page_cost = "1.1"
        }
      }

      resources = {
        requests = { cpu = "100m", memory = local.effective.postgres_memory }
        limits   = { memory = local.effective.postgres_memory }
      }

      # Fail over quickly. The default is deliberately conservative for
      # production, where a network blip should not trigger a promotion; here a
      # fast failover is the thing being demonstrated.
      failoverDelay         = 0
      switchoverDelay       = 30
      primaryUpdateStrategy = "unsupervised"
      primaryUpdateMethod   = "switchover"
      enablePDB             = local.effective.postgres_instances > 1
      stopDelay             = 30
      smartShutdownTimeout  = 15

      monitoring = {
        # Exposes a /metrics endpoint and a PodMonitor, so replica lag lands in
        # Prometheus without any extra exporter. The Day 4 alert rules depend
        # on cnpg_pg_replication_lag existing.
        enablePodMonitor = true
      }
    }
  })
}

output "postgres_app_password" {
  description = "Generated application database password."
  value       = random_password.postgres_app.result
  sensitive   = true
}
