# AWS deploy (29-day demo)

Ships the stack described on the resume:

- **ECS Fargate** behind an **ALB** in private subnets
- **ElastiCache Redis** (`cache.t4g.micro`) for budget enforcement
- **DynamoDB** for durable spend records (on-demand, TTL 90d)
- **RDS Postgres** (`db.t4g.micro`, single-AZ) for tenants/agents/budgets/api_keys

Cost: **~$78/mo** in `eu-west-2` (~$75 for the 29-day window), covered by
the $120 new-account credit. The stack tears itself down at 29 days, or
when spend is forecast past $80, whichever comes first.

## One-time account setup

1. **AWS CLI + Terraform ≥ 1.6**, logged in to the account that holds the $100 credit.
2. **GitHub OIDC role** (lets Actions push to ECR without long-lived keys):
   ```bash
   # trust policy: allow github.com/<you>/budget-governor to assume this role
   aws iam create-role --role-name bgov-github-deploy \
     --assume-role-policy-document file://deploy/aws/trust-policy.json
   aws iam attach-role-policy --role-name bgov-github-deploy \
     --policy-arn arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryPowerUser
   aws iam attach-role-policy --role-name bgov-github-deploy \
     --policy-arn arn:aws:iam::aws:policy/AmazonECS_FullAccess
   ```
   Add the role ARN as the `AWS_DEPLOY_ROLE_ARN` GitHub secret.

## First apply

```bash
cd deploy/aws/terraform
cp terraform.tfvars.example terraform.tfvars
# edit alert_email + db_password + auto_destroy_at (29 days from today)

terraform init
terraform apply
```

Expect **~12 min** on the first apply (RDS is the slow one). Copy the
`alb_dns_name` output — that's where the gateway lives.

## Push images

The GitHub Actions workflow handles this on every push to `main`. For the
first deploy, run it manually:

```bash
gh workflow run aws-deploy.yml
```

Wait for it to finish, then hit `GET /healthz` on the ALB DNS.

## Apply migrations (once)

Postgres comes up empty. Run the shared `migrations/0001_init.sql` via the
one-shot `bgov-migrate` task:

```bash
SUBNETS=$(terraform output -json | jq -r '.vpc_private_subnets.value | join(",")' 2>/dev/null \
  || aws ec2 describe-subnets --filters "Name=tag:Name,Values=bgov-private-*" --query 'Subnets[].SubnetId' --output text | tr '\t' ',')
SG=$(aws ec2 describe-security-groups --filters "Name=group-name,Values=bgov-fargate" --query 'SecurityGroups[0].GroupId' --output text)

aws ecs run-task \
  --cluster bgov-cluster \
  --task-definition bgov-migrate \
  --launch-type FARGATE \
  --network-configuration "awsvpcConfiguration={subnets=[$SUBNETS],securityGroups=[$SG]}"
```

Only needed once per fresh database. The schema also includes
`schema_migrations` so re-runs are safe.

## Seed demo tenants (once)

The controlplane is internal-only, so `seed.py` runs inside the VPC using
the same migrate image with a command override. It prints API keys, so
save the logs to the git-ignored `scripts/.seed_output.aws.txt`:

```bash
ARN=$(aws ecs run-task --cluster bgov-cluster --task-definition bgov-migrate \
  --launch-type FARGATE \
  --network-configuration "awsvpcConfiguration={subnets=[$SUBNETS],securityGroups=[$SG]}" \
  --overrides '{"containerOverrides":[{"name":"migrate","command":["python","scripts/seed.py"]}]}' \
  --query 'tasks[0].taskArn' --output text)
aws ecs wait tasks-stopped --cluster bgov-cluster --tasks "$ARN"
aws logs get-log-events --log-group-name /bgov/controlplane \
  --log-stream-name "migrate/migrate/${ARN##*/}" \
  --query 'events[].message' --output text | tr '\t' '\n' > scripts/.seed_output.aws.txt
```

The keys also sit in CloudWatch Logs for 7 days (the log group's retention).

## Smoke test

```bash
ALB=http://$(terraform -chdir=deploy/aws/terraform output -raw alb_dns_name)
curl $ALB/readyz   # {"ready":true}
```

## Load test

Run the `k6-evidence` workflow from the Actions tab (gateway URL + an
admin key from the seed output), or locally:

```bash
k6 run -e GATEWAY_URL=$ALB -e API_KEY=<acme admin key> loadtest/fanout_burst.js
```

## Teardown

Three ways the stack dies:

1. **Auto, at 29 days** — EventBridge fires `bgov-cleanup` which scales
   Fargate to 0 and deletes Postgres + Redis.
2. **Budget cap** — if spend hits $80, the same Lambda fires via SNS.
3. **Manual** — `terraform destroy` from this directory.

The auto-paths stop the expensive resources but leave the ALB, VPC and
DynamoDB table. **You still need to run `terraform destroy`** afterwards
to release everything and keep Terraform state in sync.

## What's not here (on purpose)

- **HTTPS / Route53** — the ALB listens on port 80 only. Add an ACM cert
  + HTTPS listener before anyone external touches it.
- **Multi-AZ Redis / Postgres** — single-node everywhere to hit
  the $50 figure. For real prod, add a replica and flip
  `automatic_failover_enabled`.
- **Dev/staging environments** — one stack, one workspace.
- **Container Insights** — disabled; costs more than the cluster itself
  at this scale.

## Cost breakdown

| Item | $/mo |
|---|---|
| ALB (fixed + ~1 LCU) | 24 |
| RDS Postgres `db.t4g.micro` + 20 GB gp3 | 16 |
| ElastiCache `cache.t4g.micro` | 13 |
| Public IPv4 × 3 (ALB ×2, NAT ×1) | 11 |
| ECS Fargate Spot × 3 svcs × 0.25 vCPU / 0.5 GB | 9 |
| NAT instance `t4g.nano` (fck-nat) | 4 |
| CloudWatch Logs + ECR + Secrets + SNS + DynamoDB | 3 |
| **Total** | **~$78** |

Aurora Serverless v2 was tried and dropped: the gateway's 30–60s config
refresh stops it from ever pausing, so it bills 0.5 ACU (~$55/mo) 24/7.
