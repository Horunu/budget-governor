resource "aws_ecs_cluster" "main" {
  name = "${var.name_prefix}-cluster"
  setting {
    name  = "containerInsights"
    value = "disabled"
  }
}

# shortcut: Spot only, no on-demand fallback. A reclaimed task restarts in
# ~1 min; add a FARGATE base=1 strategy if you need it to stay up mid-demo.
resource "aws_ecs_cluster_capacity_providers" "main" {
  cluster_name       = aws_ecs_cluster.main.name
  capacity_providers = ["FARGATE", "FARGATE_SPOT"]
  default_capacity_provider_strategy {
    capacity_provider = "FARGATE_SPOT"
    weight            = 1
  }
}

resource "aws_service_discovery_private_dns_namespace" "main" {
  name = "${var.name_prefix}.internal"
  vpc  = aws_vpc.main.id
}

locals {
  common_env = [
    { name = "AWS_REGION", value = var.region },
    { name = "ENVIRONMENT", value = "production" },
    { name = "REDIS_ADDR", value = "${aws_elasticache_replication_group.main.primary_endpoint_address}:6379" },
    { name = "POSTGRES_DSN", value = "postgres://budget_governor:${var.db_password}@${aws_db_instance.main.address}:5432/budget_governor?sslmode=require" },
  ]
  gateway_env = concat(local.common_env, [
    { name = "SPEND_BACKEND", value = "dynamo" },
    { name = "SPEND_DYNAMO_TABLE", value = aws_dynamodb_table.spend_events.name },
    { name = "ADVISOR_URL", value = "http://advisor.${var.name_prefix}.internal:8082" },
  ])
  # Controlplane needs the bootstrap token; the migrate image also runs seed.py against it.
  admin_env = concat(local.common_env, [
    { name = "PLATFORM_BOOTSTRAP_TOKEN", value = var.platform_token },
    { name = "CONTROLPLANE_URL", value = "http://controlplane.${var.name_prefix}.internal:8081" },
  ])
}

# ---- Gateway (public via ALB) ----

resource "aws_ecs_task_definition" "gateway" {
  family                   = "${var.name_prefix}-gateway"
  cpu                      = "256"
  memory                   = "512"
  network_mode             = "awsvpc"
  requires_compatibilities = ["FARGATE"]
  execution_role_arn       = aws_iam_role.task_execution.arn
  task_role_arn            = aws_iam_role.task.arn
  runtime_platform {
    cpu_architecture        = "X86_64"
    operating_system_family = "LINUX"
  }

  container_definitions = jsonencode([{
    name      = "gateway"
    image     = "${aws_ecr_repository.gateway.repository_url}:${var.image_tag}"
    essential = true
    portMappings = [
      { containerPort = 8080, protocol = "tcp" },
      { containerPort = 9090, protocol = "tcp" },
    ]
    environment = local.gateway_env
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        awslogs-group         = aws_cloudwatch_log_group.gateway.name
        awslogs-region        = var.region
        awslogs-stream-prefix = "gateway"
      }
    }
  }])
}

resource "aws_ecs_service" "gateway" {
  name            = "gateway"
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.gateway.arn
  desired_count   = 1
  capacity_provider_strategy {
    capacity_provider = "FARGATE_SPOT"
    weight            = 1
  }
  enable_execute_command = true

  network_configuration {
    subnets          = aws_subnet.private[*].id
    security_groups  = [aws_security_group.fargate.id]
    assign_public_ip = false
  }

  load_balancer {
    target_group_arn = aws_lb_target_group.gateway.arn
    container_name   = "gateway"
    container_port   = 8080
  }

  depends_on = [aws_lb_listener.http, aws_ecs_cluster_capacity_providers.main]
}

# ---- Advisor (internal) ----

resource "aws_ecs_task_definition" "advisor" {
  family                   = "${var.name_prefix}-advisor"
  cpu                      = "256"
  memory                   = "512"
  network_mode             = "awsvpc"
  requires_compatibilities = ["FARGATE"]
  execution_role_arn       = aws_iam_role.task_execution.arn
  task_role_arn            = aws_iam_role.task.arn
  runtime_platform {
    cpu_architecture        = "X86_64"
    operating_system_family = "LINUX"
  }

  container_definitions = jsonencode([{
    name         = "advisor"
    image        = "${aws_ecr_repository.advisor.repository_url}:${var.image_tag}"
    essential    = true
    portMappings = [{ containerPort = 8082, protocol = "tcp" }]
    environment  = local.common_env
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        awslogs-group         = aws_cloudwatch_log_group.advisor.name
        awslogs-region        = var.region
        awslogs-stream-prefix = "advisor"
      }
    }
  }])
}

resource "aws_service_discovery_service" "advisor" {
  name = "advisor"
  dns_config {
    namespace_id = aws_service_discovery_private_dns_namespace.main.id
    dns_records {
      type = "A"
      ttl  = 10
    }
  }
}

resource "aws_ecs_service" "advisor" {
  name            = "advisor"
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.advisor.arn
  desired_count   = 1
  capacity_provider_strategy {
    capacity_provider = "FARGATE_SPOT"
    weight            = 1
  }

  network_configuration {
    subnets          = aws_subnet.private[*].id
    security_groups  = [aws_security_group.fargate.id]
    assign_public_ip = false
  }

  service_registries {
    registry_arn = aws_service_discovery_service.advisor.arn
  }

  depends_on = [aws_ecs_cluster_capacity_providers.main]
}

# ---- One-shot migrate task (run via `aws ecs run-task`; see docs) ----

resource "aws_ecs_task_definition" "migrate" {
  family                   = "${var.name_prefix}-migrate"
  cpu                      = "256"
  memory                   = "512"
  network_mode             = "awsvpc"
  requires_compatibilities = ["FARGATE"]
  execution_role_arn       = aws_iam_role.task_execution.arn
  task_role_arn            = aws_iam_role.task.arn
  runtime_platform {
    cpu_architecture        = "X86_64"
    operating_system_family = "LINUX"
  }

  container_definitions = jsonencode([{
    name        = "migrate"
    image       = "${aws_ecr_repository.migrate.repository_url}:${var.image_tag}"
    essential   = true
    environment = local.admin_env
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        awslogs-group         = aws_cloudwatch_log_group.controlplane.name
        awslogs-region        = var.region
        awslogs-stream-prefix = "migrate"
      }
    }
  }])
}

# ---- Controlplane (internal; reachable via SSM tunnel for admin) ----

resource "aws_ecs_task_definition" "controlplane" {
  family                   = "${var.name_prefix}-controlplane"
  cpu                      = "256"
  memory                   = "512"
  network_mode             = "awsvpc"
  requires_compatibilities = ["FARGATE"]
  execution_role_arn       = aws_iam_role.task_execution.arn
  task_role_arn            = aws_iam_role.task.arn
  runtime_platform {
    cpu_architecture        = "X86_64"
    operating_system_family = "LINUX"
  }

  container_definitions = jsonencode([{
    name         = "controlplane"
    image        = "${aws_ecr_repository.controlplane.repository_url}:${var.image_tag}"
    essential    = true
    portMappings = [{ containerPort = 8081, protocol = "tcp" }]
    environment  = local.admin_env
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        awslogs-group         = aws_cloudwatch_log_group.controlplane.name
        awslogs-region        = var.region
        awslogs-stream-prefix = "controlplane"
      }
    }
  }])
}

resource "aws_service_discovery_service" "controlplane" {
  name = "controlplane"
  dns_config {
    namespace_id = aws_service_discovery_private_dns_namespace.main.id
    dns_records {
      type = "A"
      ttl  = 10
    }
  }
}

resource "aws_ecs_service" "controlplane" {
  name            = "controlplane"
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.controlplane.arn
  desired_count   = 1
  capacity_provider_strategy {
    capacity_provider = "FARGATE_SPOT"
    weight            = 1
  }

  network_configuration {
    subnets          = aws_subnet.private[*].id
    security_groups  = [aws_security_group.fargate.id]
    assign_public_ip = false
  }

  service_registries {
    registry_arn = aws_service_discovery_service.controlplane.arn
  }

  depends_on = [aws_ecs_cluster_capacity_providers.main]
}
