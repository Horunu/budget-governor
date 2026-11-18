output "alb_dns_name" {
  value       = aws_lb.gateway.dns_name
  description = "Public DNS of the gateway's ALB. Point clients here."
}

output "ecr_repos" {
  value = {
    gateway      = aws_ecr_repository.gateway.repository_url
    controlplane = aws_ecr_repository.controlplane.repository_url
    advisor      = aws_ecr_repository.advisor.repository_url
  }
  description = "Push images here from CI."
}

output "dynamodb_spend_table" {
  value = aws_dynamodb_table.spend_events.name
}

output "db_endpoint" {
  value = aws_db_instance.main.address
}

output "redis_endpoint" {
  value = aws_elasticache_replication_group.main.primary_endpoint_address
}

output "auto_destroy_at" {
  value       = var.auto_destroy_at
  description = "When the cleanup Lambda will run. Run `terraform destroy` manually after to free the ALB and VPC."
}
