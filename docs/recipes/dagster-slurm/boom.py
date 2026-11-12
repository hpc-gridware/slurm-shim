"""Cluster payload that fails, to prove a failed job fails the Dagster run."""
import sys

print("intentional failure", file=sys.stderr)
sys.exit(3)
