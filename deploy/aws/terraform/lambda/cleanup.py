"""Tear-down Lambda. Fired by (a) EventBridge at auto_destroy_at or
(b) the $80 AWS Budget action. Scales Fargate services to 0, deletes the
RDS Postgres instance and Redis replication group, notifies on SNS. The ALB,
VPC, DynamoDB table and ECR repos are left for a final manual
``terraform destroy`` so Terraform state and the real world stay in sync.

shortcut: no idempotency beyond AWS's "already deleted" errors. Re-firing
is harmless but logs look messy."""
import os
import boto3
import botocore

CLUSTER = os.environ["ECS_CLUSTER"]
DB_INSTANCE = os.environ["DB_INSTANCE"]
REDIS_GROUP = os.environ["REDIS_GROUP"]
# Separate from the topic that triggers this Lambda, so the notice can't re-invoke it.
SNS_TOPIC = os.environ["NOTICE_TOPIC_ARN"]

ecs = boto3.client("ecs")
rds = boto3.client("rds")
ec = boto3.client("elasticache")
sns = boto3.client("sns")


def _scale_fargate_to_zero():
    svcs = ecs.list_services(cluster=CLUSTER).get("serviceArns", [])
    for arn in svcs:
        try:
            ecs.update_service(cluster=CLUSTER, service=arn, desiredCount=0)
            print(f"scaled {arn} -> 0")
        except botocore.exceptions.ClientError as e:
            print(f"scale {arn} failed: {e}")


def _delete_db():
    try:
        rds.delete_db_instance(
            DBInstanceIdentifier=DB_INSTANCE,
            SkipFinalSnapshot=True,
            DeleteAutomatedBackups=True,
        )
        print(f"deleted rds instance {DB_INSTANCE}")
    except botocore.exceptions.ClientError as e:
        print(f"rds delete failed: {e}")


def _delete_redis():
    try:
        ec.delete_replication_group(
            ReplicationGroupId=REDIS_GROUP,
            RetainPrimaryCluster=False,
        )
        print(f"deleted redis group {REDIS_GROUP}")
    except botocore.exceptions.ClientError as e:
        print(f"redis delete failed: {e}")


def handler(event, _context):
    trigger = event.get("source", "unknown")
    print(f"cleanup fired by: {trigger}")
    _scale_fargate_to_zero()
    _delete_db()
    _delete_redis()
    sns.publish(
        TopicArn=SNS_TOPIC,
        Subject="budget-governor: AWS teardown fired",
        Message=(
            f"Trigger: {trigger}\n"
            f"Fargate services scaled to 0; Postgres + Redis delete requested.\n"
            f"Run `terraform destroy` to free the ALB, VPC and remaining resources."
        ),
    )
    return {"ok": True}
