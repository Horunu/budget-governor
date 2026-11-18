# ---- SNS topic (budget alerts + Lambda notifications) ----

resource "aws_sns_topic" "alerts" {
  name = "${var.name_prefix}-alerts"
}

resource "aws_sns_topic_policy" "alerts" {
  arn = aws_sns_topic.alerts.arn
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "budgets.amazonaws.com" }
      Action    = "SNS:Publish"
      Resource  = aws_sns_topic.alerts.arn
    }]
  })
}

# The cleanup Lambda reports here, not on `alerts`, so it can't trigger itself.
resource "aws_sns_topic" "notices" {
  name = "${var.name_prefix}-notices"
}

resource "aws_sns_topic_subscription" "email" {
  topic_arn = aws_sns_topic.notices.arn
  protocol  = "email"
  endpoint  = var.alert_email
}

# ---- $80 budget cap ----
# At 80% a warning email fires; at 100% the cleanup Lambda runs via SNS
# → Lambda subscription. shortcut: AWS Budgets lags cost data by 6-24
# hours, so the 29-day timer below is the real guarantee.

resource "aws_budgets_budget" "cap" {
  name         = "${var.name_prefix}-cap"
  budget_type  = "COST"
  limit_amount = tostring(var.budget_cap_usd)
  limit_unit   = "USD"
  time_unit    = "MONTHLY"

  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = 80
    threshold_type             = "PERCENTAGE"
    notification_type          = "ACTUAL"
    subscriber_email_addresses = [var.alert_email]
  }
  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = 100
    threshold_type             = "PERCENTAGE"
    notification_type          = "FORECASTED"
    subscriber_sns_topic_arns  = [aws_sns_topic.alerts.arn]
    subscriber_email_addresses = [var.alert_email]
  }
}

# ---- Cleanup Lambda ----

data "archive_file" "cleanup" {
  type        = "zip"
  source_file = "${path.module}/lambda/cleanup.py"
  output_path = "${path.module}/.build/cleanup.zip"
}

resource "aws_lambda_function" "cleanup" {
  function_name    = "${var.name_prefix}-cleanup"
  filename         = data.archive_file.cleanup.output_path
  source_code_hash = data.archive_file.cleanup.output_base64sha256
  role             = aws_iam_role.cleanup.arn
  handler          = "cleanup.handler"
  runtime          = "python3.12"
  timeout          = 60

  environment {
    variables = {
      ECS_CLUSTER      = aws_ecs_cluster.main.name
      DB_INSTANCE      = aws_db_instance.main.identifier
      REDIS_GROUP      = aws_elasticache_replication_group.main.id
      NOTICE_TOPIC_ARN = aws_sns_topic.notices.arn
    }
  }

  depends_on = [aws_cloudwatch_log_group.cleanup]
}

# SNS → Lambda so the budget-cap-at-100% notification triggers teardown.
resource "aws_sns_topic_subscription" "cleanup" {
  topic_arn = aws_sns_topic.alerts.arn
  protocol  = "lambda"
  endpoint  = aws_lambda_function.cleanup.arn
}

resource "aws_lambda_permission" "sns" {
  statement_id  = "AllowSNSInvoke"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.cleanup.function_name
  principal     = "sns.amazonaws.com"
  source_arn    = aws_sns_topic.alerts.arn
}

# ---- 29-day auto-destruct (EventBridge Scheduler) ----

resource "aws_iam_role" "scheduler" {
  name = "${var.name_prefix}-scheduler"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Action    = "sts:AssumeRole"
      Principal = { Service = "scheduler.amazonaws.com" }
    }]
  })
}

resource "aws_iam_role_policy" "scheduler_invoke" {
  role = aws_iam_role.scheduler.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = "lambda:InvokeFunction"
      Resource = aws_lambda_function.cleanup.arn
    }]
  })
}

resource "aws_scheduler_schedule" "auto_destroy" {
  name                         = "${var.name_prefix}-auto-destroy"
  schedule_expression          = "at(${replace(var.auto_destroy_at, "Z", "")})"
  schedule_expression_timezone = "UTC"
  flexible_time_window { mode = "OFF" }

  target {
    arn      = aws_lambda_function.cleanup.arn
    role_arn = aws_iam_role.scheduler.arn
    input    = jsonencode({ source = "scheduler.auto_destroy" })
  }
}
