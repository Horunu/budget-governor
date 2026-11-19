variable "region" {
  type    = string
  default = "eu-west-2"
}

variable "name_prefix" {
  type    = string
  default = "bgov"
}

variable "image_tag" {
  type        = string
  default     = "latest"
  description = "Container image tag deployed to all three Fargate services. The CI workflow pushes tags on main."
}

variable "alert_email" {
  type        = string
  description = "Address that receives the $80 budget alarm and the 29-day auto-destruct notice."
}

variable "budget_cap_usd" {
  type        = number
  default     = 80
  description = "Hard spend ceiling; at 100%% the cleanup Lambda fires via SNS."
}

variable "auto_destroy_at" {
  type        = string
  default     = "2026-12-17T00:00:00Z"
  description = "UTC timestamp when the cleanup Lambda scales Fargate to 0 and tears down Aurora+ElastiCache. Set 29 days from `terraform apply` time."
}

variable "platform_token" {
  type        = string
  sensitive   = true
  description = "Controlplane PLATFORM_BOOTSTRAP_TOKEN, required to create tenants (scripts/seed.py)."
}

variable "db_password" {
  type        = string
  sensitive   = true
  description = "Initial master password for Aurora Postgres. Stored in Secrets Manager after apply."
}
