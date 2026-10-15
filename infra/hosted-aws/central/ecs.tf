resource "aws_cloudwatch_log_group" "central" {
  name              = "/iterabase/central"
  retention_in_days = 90
}

resource "aws_ecs_cluster" "central" {
  name = local.name

  # RunningTaskCount for the replica alarm.
  setting {
    name  = "containerInsights"
    value = "enabled"
  }
}

resource "aws_ecs_task_definition" "central" {
  family                   = local.name
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = "512"
  memory                   = "1024"
  execution_role_arn       = aws_iam_role.execution.arn
  task_role_arn            = aws_iam_role.task.arn

  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "X86_64"
  }

  container_definitions = jsonencode([{
    name      = "central"
    image     = local.image
    essential = true

    # Migrations run under an advisory lock before serving, so the two
    # replicas of a rollout cannot race (docs/architecture/v2-central-inference-gateway.md).
    entryPoint = ["/bin/sh", "-c"]
    command    = ["central migrate up && exec central serve"]

    portMappings = [
      { containerPort = local.listen_port, protocol = "tcp" },
      { containerPort = local.health_port, protocol = "tcp" },
    ]

    environment = [
      { name = "AWS_REGION", value = var.region },
      { name = "CENTRAL_CONFIG", value = local.central_config },
      { name = "CENTRAL_OPERATORS", value = jsonencode(local.operators) },
      { name = "CENTRAL_PUBLIC_HOST", value = local.public_host },
      { name = "CENTRAL_KMS_KEY_ID", value = aws_kms_key.ca.arn },
      { name = "CENTRAL_ALERT_TOPIC_ARN", value = aws_sns_topic.billing_alerts.arn },
      { name = "CENTRAL_DATABASE_HOST", value = aws_db_instance.central.address },
      { name = "CENTRAL_DATABASE_PORT", value = tostring(aws_db_instance.central.port) },
      { name = "CENTRAL_DATABASE_NAME", value = aws_db_instance.central.db_name },
      { name = "CENTRAL_DATABASE_USER", value = aws_db_instance.central.username },
      { name = "CENTRAL_DATABASE_SSLMODE", value = "require" },
    ]

    secrets = [
      { name = "CENTRAL_PROVIDER_KEY", valueFrom = aws_secretsmanager_secret.provider_key.arn },
      { name = "CENTRAL_DATABASE_PASSWORD", valueFrom = "${aws_db_instance.central.master_user_secret[0].secret_arn}:password::" },
    ]

    logConfiguration = {
      logDriver = "awslogs"
      options = {
        awslogs-group         = aws_cloudwatch_log_group.central.name
        awslogs-region        = var.region
        awslogs-stream-prefix = "central"
      }
    }
  }])

  lifecycle {
    precondition {
      condition     = var.central_image_digest != local.unreleased_digest
      error_message = "Set central_image_digest in release.auto.tfvars.json to the first published inference-gateway release that ships cmd/central."
    }
  }
}

resource "aws_ecs_service" "central" {
  name            = local.name
  cluster         = aws_ecs_cluster.central.id
  task_definition = aws_ecs_task_definition.central.arn
  desired_count   = var.task_count
  launch_type     = "FARGATE"

  deployment_minimum_healthy_percent = 50
  deployment_maximum_percent         = 200
  health_check_grace_period_seconds  = 60

  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

  network_configuration {
    subnets          = local.subnet_ids
    security_groups  = [aws_security_group.task.id]
    assign_public_ip = true
  }

  load_balancer {
    target_group_arn = aws_lb_target_group.central.arn
    container_name   = "central"
    container_port   = local.listen_port
  }

  depends_on = [aws_lb_listener.central]
}
