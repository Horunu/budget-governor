# ---- ECR ----

resource "aws_ecr_repository" "gateway" {
  name                 = "${var.name_prefix}/gateway"
  image_tag_mutability = "MUTABLE"
  force_delete         = true
  image_scanning_configuration { scan_on_push = true }
}

resource "aws_ecr_repository" "controlplane" {
  name                 = "${var.name_prefix}/controlplane"
  image_tag_mutability = "MUTABLE"
  force_delete         = true
  image_scanning_configuration { scan_on_push = true }
}

resource "aws_ecr_repository" "advisor" {
  name                 = "${var.name_prefix}/advisor"
  image_tag_mutability = "MUTABLE"
  force_delete         = true
  image_scanning_configuration { scan_on_push = true }
}

resource "aws_ecr_repository" "migrate" {
  name                 = "${var.name_prefix}/migrate"
  image_tag_mutability = "MUTABLE"
  force_delete         = true
  image_scanning_configuration { scan_on_push = true }
}

# Keep only the two most recent images so ECR storage stays under the
# always-free 500 MB.
resource "aws_ecr_lifecycle_policy" "trim" {
  for_each   = { gw = aws_ecr_repository.gateway.name, cp = aws_ecr_repository.controlplane.name, ad = aws_ecr_repository.advisor.name, mg = aws_ecr_repository.migrate.name }
  repository = each.value
  policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "keep 2 most recent"
      selection    = { tagStatus = "any", countType = "imageCountMoreThan", countNumber = 2 }
      action       = { type = "expire" }
    }]
  })
}

# ---- DynamoDB ----
# Primary access pattern: a tenant's spend over a time window. PK
# tenant_id + SK ts#trace_id makes that a Query with a BETWEEN condition.
# On-demand so cold periods stay within the 25 WCU / 25 RCU always-free.

resource "aws_dynamodb_table" "spend_events" {
  name         = "${var.name_prefix}-spend-events"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "tenant_id"
  range_key    = "ts_trace"

  attribute {
    name = "tenant_id"
    type = "S"
  }
  attribute {
    name = "ts_trace"
    type = "S"
  }

  ttl {
    attribute_name = "ttl"
    enabled        = true
  }

  point_in_time_recovery { enabled = false }
}

# ---- RDS Postgres (single-AZ db.t4g.micro) ----
# Aurora Serverless v2 was dropped: the gateway's 30-60s config/auth
# refresh keeps it from ever pausing, so it bills 0.5 ACU (~$55/mo) 24/7.

resource "aws_db_subnet_group" "postgres" {
  name       = "${var.name_prefix}-postgres"
  subnet_ids = aws_subnet.private[*].id
}

resource "aws_db_instance" "main" {
  identifier              = "${var.name_prefix}-postgres"
  engine                  = "postgres"
  engine_version          = "16"
  instance_class          = "db.t4g.micro"
  allocated_storage       = 20
  storage_type            = "gp3"
  db_name                 = "budget_governor"
  username                = "budget_governor"
  password                = var.db_password
  db_subnet_group_name    = aws_db_subnet_group.postgres.name
  vpc_security_group_ids  = [aws_security_group.postgres.id]
  publicly_accessible     = false
  multi_az                = false
  backup_retention_period = 0
  skip_final_snapshot     = true
  apply_immediately       = true
}

# ---- ElastiCache Redis (single-node t4g.micro) ----
# shortcut: single node, no replica. Add automatic_failover_enabled + a
# replica node to validate the resume's "fail-closed on failover" claim
# for real -- today that claim is proved by miniredis in the unit tests.

resource "aws_elasticache_subnet_group" "main" {
  name       = "${var.name_prefix}-redis"
  subnet_ids = aws_subnet.private[*].id
}

resource "aws_elasticache_replication_group" "main" {
  replication_group_id       = "${var.name_prefix}-redis"
  description                = "budget-governor Redis"
  engine                     = "redis"
  engine_version             = "7.1"
  node_type                  = "cache.t4g.micro"
  num_cache_clusters         = 1
  automatic_failover_enabled = false
  port                       = 6379
  subnet_group_name          = aws_elasticache_subnet_group.main.name
  security_group_ids         = [aws_security_group.redis.id]
  apply_immediately          = true
}

# ---- Secrets ----

resource "aws_secretsmanager_secret" "db_password" {
  name                    = "${var.name_prefix}/db-password"
  recovery_window_in_days = 0
}

resource "aws_secretsmanager_secret_version" "db_password" {
  secret_id     = aws_secretsmanager_secret.db_password.id
  secret_string = var.db_password
}

# ---- CloudWatch log groups ----

resource "aws_cloudwatch_log_group" "gateway" {
  name              = "/${var.name_prefix}/gateway"
  retention_in_days = 7
}

resource "aws_cloudwatch_log_group" "controlplane" {
  name              = "/${var.name_prefix}/controlplane"
  retention_in_days = 7
}

resource "aws_cloudwatch_log_group" "advisor" {
  name              = "/${var.name_prefix}/advisor"
  retention_in_days = 7
}

resource "aws_cloudwatch_log_group" "cleanup" {
  name              = "/${var.name_prefix}/cleanup"
  retention_in_days = 30
}
