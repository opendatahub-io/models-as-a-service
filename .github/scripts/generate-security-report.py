#!/usr/bin/env python3
# Plugin Version: 2.3.0
# DO NOT REMOVE: This comment is used for version detection during plugin updates
"""
Comprehensive Security Report Generator

Parses outputs from all security scanning tools and generates a detailed
markdown report suitable for security review and JIRA attachments.

Usage:
    python .github/scripts/generate-security-report.py --output security-report.md

Tools parsed:
    - Gitleaks (JSON)
    - TruffleHog (JSON)
    - Semgrep (SARIF)
    - ShellCheck (JSON)
    - Hadolint (SARIF)
    - yamllint (text)
    - kube-linter (JSON)
    - RBAC Analyzer (text)
"""

import json
import os
import sys
import argparse
import re
import hashlib
import yaml
from datetime import datetime, timezone
from pathlib import Path
from typing import Dict, List, Any, Optional


# Report Format Version: 1.0
# Breaking changes require updating .github/workflows/security-full-scan.yml badge parsing
class SecurityReportGenerator:
    def __init__(self, workspace: str, github_context: Dict[str, str], yamllint_limit: int = 50):
        self.workspace = Path(workspace)
        self.github = github_context
        self.yamllint_limit = yamllint_limit
        self.findings = {
            'critical': [],
            'high': [],
            'medium': [],
            'low': [],
            'info': []
        }
        self.tool_stats = {}
        self.baseline = {}  # Loaded from .github/config/security-baseline.yaml
        # Initialize baseline_counts for all tools to prevent KeyError in standalone usage
        self.baseline_counts = {t: 0 for t in [
            'gitleaks', 'trufflehog', 'semgrep', 'shellcheck', 'hadolint',
            'yamllint', 'actionlint', 'kube-linter', 'rbac-analyzer'
        ]}

    def load_baseline(self) -> None:
        """Load acknowledged findings baseline from GitHub Secrets (mandatory)

        This filters out findings that teams have acknowledged as false positives
        or accepted risks. The baseline contains detailed justifications.

        Requires:
            SECURITY_BASELINE environment variable (loaded from GitHub Secrets by workflow)

        Raises:
            SystemExit: If SECURITY_BASELINE environment variable is not set
        """
        # Reset baseline counts for this scan (already initialized in __init__)
        for tool in self.baseline_counts:
            self.baseline_counts[tool] = 0

        # Check for SECURITY_BASELINE environment variable (mandatory)
        baseline_yaml_str = os.getenv('SECURITY_BASELINE')
        if not baseline_yaml_str:
            print(f"[ERROR] SECURITY_BASELINE environment variable not set", file=sys.stderr)
            print(f"[ERROR] Baseline must be loaded from GitHub Secrets via workflow", file=sys.stderr)
            print(f"[ERROR] ", file=sys.stderr)
            print(f"[ERROR] This should have been set by the 'Load baseline from GitHub Secret' step", file=sys.stderr)
            print(f"[ERROR] Check workflow logs for baseline loading failures", file=sys.stderr)
            sys.exit(1)

        # Parse baseline from environment variable
        try:
            baseline_data = yaml.safe_load(baseline_yaml_str)
            print(f"[INFO] Loaded baseline from GitHub Secret (SECURITY_BASELINE env var)", file=sys.stderr)
        except Exception as e:
            print(f"[ERROR] Failed to parse baseline from env var: {str(e)}", file=sys.stderr)
            print(f"[ERROR] Baseline YAML is invalid", file=sys.stderr)
            sys.exit(1)

        if baseline_data is None:
            print(f"[INFO] Baseline is empty - all findings will be reported", file=sys.stderr)
            return

        # Validate baseline version
        version = baseline_data.get('version', '1.0')
        if version != '2.0':
            print(f"[WARNING] Unexpected baseline version: {version}, expected 2.0", file=sys.stderr)

        # Store baseline data for each tool
        for tool in self.baseline_counts:
            self.baseline[tool] = baseline_data.get(tool, [])

    def _is_acknowledged(self, tool: str, finding: Dict[str, Any]) -> bool:
        """Check if a finding matches an acknowledged baseline entry

        Args:
            tool: Tool name (gitleaks, trufflehog, semgrep, etc.)
            finding: Finding dict from parser (contains tool-specific fields)

        Returns:
            True if finding is acknowledged in baseline, False otherwise
        """
        if tool not in self.baseline or not self.baseline[tool]:
            return False

        tool_baseline = self.baseline[tool]

        # Tool-specific matching logic (matches acknowledge-findings.py)
        for baseline_entry in tool_baseline:
            if tool == 'gitleaks':
                # Gitleaks findings need description hash for uniqueness
                # Calculate it from finding description
                description = finding.get('description', '')
                desc_hash = hashlib.sha256(description.encode()).hexdigest()[:8]

                if (finding.get('file') == baseline_entry.get('file') and
                    str(finding.get('line')) == str(baseline_entry.get('line')) and
                    finding.get('rule') == baseline_entry.get('rule') and
                    desc_hash == baseline_entry.get('description_hash')):
                    return True

            elif tool == 'trufflehog':
                if (finding.get('detector') == baseline_entry.get('detector') and
                    finding.get('file') == baseline_entry.get('file') and
                    str(finding.get('line')) == str(baseline_entry.get('line'))):
                    return True

            elif tool == 'semgrep' or tool == 'hadolint':
                if (finding.get('rule') == baseline_entry.get('rule_id') and
                    finding.get('file') == baseline_entry.get('file') and
                    str(finding.get('line')) == str(baseline_entry.get('line'))):
                    return True

            elif tool == 'shellcheck':
                if (finding.get('file') == baseline_entry.get('file') and
                    str(finding.get('line')) == str(baseline_entry.get('line')) and
                    finding.get('code') == baseline_entry.get('code')):
                    return True

            elif tool == 'yamllint':
                if (finding.get('file') == baseline_entry.get('file') and
                    str(finding.get('line')) == str(baseline_entry.get('line')) and
                    finding.get('rule') == baseline_entry.get('rule')):
                    return True

            elif tool == 'actionlint':
                if (finding.get('file') == baseline_entry.get('file') and
                    str(finding.get('line')) == str(baseline_entry.get('line')) and
                    finding.get('message') == baseline_entry.get('message')):
                    return True

            elif tool == 'kube-linter':
                # kube-linter uses object kind/name/namespace + check name
                obj = baseline_entry.get('object', {})
                if (finding.get('check') == baseline_entry.get('check') and
                    finding.get('object_kind') == obj.get('kind') and
                    finding.get('object_name') == obj.get('name') and
                    finding.get('object_namespace') == obj.get('namespace')):
                    return True

            elif tool == 'rbac-analyzer':
                if (finding.get('title') == baseline_entry.get('title') and
                    finding.get('file') == baseline_entry.get('file')):
                    return True

        return False

    def parse_gitleaks(self, filepath: str) -> Dict[str, Any]:
        """Parse Gitleaks JSON output"""
        stats = {'tool': 'Gitleaks', 'findings': 0, 'status': '✅ PASS'}

        if not Path(filepath).exists():
            stats['status'] = '⏭️ SKIPPED'
            return stats

        try:
            with open(filepath) as f:
                data = json.load(f)
                if data:
                    # Deduplicate findings by file:line:rule combination
                    seen = set()
                    unique_findings = []

                    for finding in data:
                        # Strip /repo/ prefix from Docker container mount path
                        file_path = finding.get('File', 'unknown')
                        if file_path.startswith('/repo/'):
                            file_path = file_path[6:]  # Remove '/repo/' prefix

                        # Normalize path using os.path.normpath for robust handling
                        file_path = os.path.normpath(file_path).lstrip('/')
                        # Ensure no leading path traversal after normalization
                        while file_path.startswith('../') or file_path.startswith('./'):
                            if file_path.startswith('../'):
                                file_path = file_path[3:]
                            elif file_path.startswith('./'):
                                file_path = file_path[2:]

                        # Include description hash to differentiate multiple secrets at same location
                        description = finding.get('Description', 'Secret detected')
                        desc_hash = hashlib.sha256(description.encode()).hexdigest()[:8]
                        dedup_key = f"{file_path}:{finding.get('StartLine', '?')}:{finding.get('RuleID', 'unknown')}:{desc_hash}"

                        if dedup_key not in seen:
                            seen.add(dedup_key)
                            finding_dict = {
                                'tool': 'Gitleaks',
                                'type': 'Hardcoded Secret',
                                'severity': 'CRITICAL',
                                'file': file_path,
                                'line': finding.get('StartLine', '?'),
                                'rule': finding.get('RuleID', 'unknown'),
                                'description': finding.get(
                                    'Description',
                                    'Secret detected; see Gitleaks JSON artifact for details (value redacted)'
                                ),
                                'remediation': 'Remove secret from code, rotate credential, use secret manager'
                            }

                            # Check if this finding is acknowledged in baseline
                            if not self._is_acknowledged('gitleaks', finding_dict):
                                unique_findings.append(finding_dict)
                            else:
                                self.baseline_counts['gitleaks'] += 1

                    self.findings['critical'].extend(unique_findings)
                    stats['findings'] = len(unique_findings)
                    if unique_findings:
                        stats['status'] = '❌ FINDINGS'

        except Exception as e:
            stats['status'] = '⚠️ ERROR: Failed to parse Gitleaks output'
            print(f"[ERROR] Gitleaks parser: {str(e)}", file=sys.stderr)

        return stats

    def parse_trufflehog(self, filepath: str) -> Dict[str, Any]:
        """Parse TruffleHog JSON output"""
        stats = {'tool': 'TruffleHog', 'findings': 0, 'status': '✅ PASS'}

        if not Path(filepath).exists():
            stats['status'] = '⏭️ SKIPPED'
            return stats

        try:
            findings_count = 0
            parse_errors = 0
            with open(filepath) as f:
                # TruffleHog outputs JSON Lines (one object per line) when findings exist,
                # but the local scanner writes "[]" when no findings are found.
                # Handle both formats gracefully.
                content = f.read().strip()
                if not content or content == '[]':
                    return stats

                # Try parsing as JSON array first (local scanner empty output)
                try:
                    parsed = json.loads(content)
                    if isinstance(parsed, list):
                        if not parsed:
                            return stats
                        lines = [json.dumps(item) for item in parsed]
                    else:
                        lines = [content]
                except json.JSONDecodeError:
                    lines = content.split('\n')

                for line in lines:
                    if line.strip():
                        try:
                            finding = json.loads(line)
                        except json.JSONDecodeError:
                            parse_errors += 1
                            continue
                        # Skip if parsed result is a list (e.g. empty array line)
                        if isinstance(finding, list):
                            continue
                        # Extract file and line from nested structure
                        fs_data = finding.get('SourceMetadata', {}).get('Data', {}).get('Filesystem', {})
                        file_path = fs_data.get('file', 'unknown')

                        # Normalize file path for consistent baseline matching
                        # TruffleHog paths can be absolute or container-prefixed
                        if file_path.startswith('/repo/'):
                            file_path = file_path[6:]
                        file_path = os.path.normpath(file_path).lstrip('/')
                        while file_path.startswith('../') or file_path.startswith('./'):
                            if file_path.startswith('../'):
                                file_path = file_path[3:]
                            elif file_path.startswith('./'):
                                file_path = file_path[2:]

                        line_num = fs_data.get('line', 0)
                        detector = finding.get('DetectorName', 'unknown')

                        finding_dict = {
                            'tool': 'TruffleHog',
                            'type': 'Verified Credential',
                            'severity': 'CRITICAL',
                            'file': file_path,
                            'line': line_num,  # Preserve numeric value for baseline matching (0, not '?')
                            'detector': detector,  # For baseline matching
                            'rule': detector,
                            'description': f"Verified {detector} found",
                            'remediation': 'URGENT: Rotate this credential immediately - it has been verified as active'
                        }

                        # Check if this finding is acknowledged in baseline
                        if not self._is_acknowledged('trufflehog', finding_dict):
                            self.findings['critical'].append(finding_dict)
                            findings_count += 1
                        else:
                            self.baseline_counts['trufflehog'] += 1

            stats['findings'] = findings_count
            if findings_count > 0:
                stats['status'] = '❌ FINDINGS'
            if parse_errors > 0:
                # Surface partial parse issues without hiding real findings
                if findings_count > 0:
                    stats['status'] = f"❌ FINDINGS (partial: {parse_errors} unparsable lines)"
                else:
                    stats['status'] = f"⚠️ PARTIAL: {parse_errors} unparsable lines"

        except Exception as e:
            stats['status'] = '⚠️ ERROR: Failed to parse TruffleHog output'
            print(f"[ERROR] TruffleHog parser: {str(e)}", file=sys.stderr)

        return stats

    def parse_semgrep_sarif(self, filepath: str) -> Dict[str, Any]:
        """Parse Semgrep SARIF output"""
        stats = {'tool': 'Semgrep', 'findings': 0, 'status': '✅ PASS'}

        if not Path(filepath).exists():
            stats['status'] = '⏭️ SKIPPED'
            return stats

        try:
            with open(filepath) as f:
                sarif = json.load(f)

            for run in sarif.get('runs', []):
                for result in run.get('results', []):
                    level = result.get('level', 'note')
                    severity_map = {
                        'error': 'high',
                        'warning': 'medium',
                        'note': 'info'
                    }
                    severity = severity_map.get(level, 'info')

                    rule = result.get('ruleId', 'unknown')
                    message = result.get('message', {}).get('text', 'No description')

                    locations = result.get('locations') or []
                    location = locations[0] if locations else {}
                    artifact = location.get('physicalLocation', {}).get('artifactLocation', {})
                    file_path = artifact.get('uri', 'unknown')

                    region = location.get('physicalLocation', {}).get('region', {})
                    line = region.get('startLine', '?')

                    finding_dict = {
                        'tool': 'Semgrep',
                        'type': rule,
                        'severity': severity.upper(),
                        'file': file_path,
                        'line': line,
                        'rule': rule,
                        'description': message,
                        'remediation': self._get_semgrep_remediation(rule)
                    }

                    # Check if this finding is acknowledged in baseline
                    if not self._is_acknowledged('semgrep', finding_dict):
                        self.findings[severity].append(finding_dict)
                        stats['findings'] += 1
                    else:
                        self.baseline_counts['semgrep'] += 1

            if stats['findings'] > 0:
                stats['status'] = '❌ FINDINGS'

        except Exception as e:
            stats['status'] = '⚠️ ERROR: Failed to parse Semgrep SARIF output'
            print(f"[ERROR] Semgrep parser: {str(e)}", file=sys.stderr)

        return stats

    def parse_hadolint_sarif(self, filepath: str) -> Dict[str, Any]:
        """Parse Hadolint SARIF output"""
        stats = {'tool': 'Hadolint', 'findings': 0, 'status': '✅ PASS'}

        if not Path(filepath).exists():
            stats['status'] = '⏭️ SKIPPED'
            return stats

        try:
            with open(filepath) as f:
                sarif = json.load(f)

            for run in sarif.get('runs', []):
                for result in run.get('results', []):
                    level = result.get('level', 'note')
                    severity_map = {
                        'error': 'high',
                        'warning': 'medium',
                        'note': 'low'
                    }
                    severity = severity_map.get(level, 'low')

                    rule = result.get('ruleId', 'unknown')
                    message = result.get('message', {}).get('text', 'No description')

                    locations = result.get('locations') or []
                    location = locations[0] if locations else {}
                    artifact = location.get('physicalLocation', {}).get('artifactLocation', {})
                    file_path = artifact.get('uri', 'unknown')

                    region = location.get('physicalLocation', {}).get('region', {})
                    line = region.get('startLine', '?')

                    finding_dict = {
                        'tool': 'Hadolint',
                        'type': 'Dockerfile Issue',
                        'severity': severity.upper(),
                        'file': file_path,
                        'line': line,
                        'rule': rule,
                        'description': message,
                        'remediation': 'Follow Dockerfile best practices and CIS benchmarks'
                    }

                    # Check if this finding is acknowledged in baseline
                    if not self._is_acknowledged('hadolint', finding_dict):
                        self.findings[severity].append(finding_dict)
                        stats['findings'] += 1
                    else:
                        self.baseline_counts['hadolint'] += 1

            if stats['findings'] > 0:
                stats['status'] = '❌ FINDINGS'

        except Exception as e:
            stats['status'] = '⚠️ ERROR: Failed to parse Hadolint SARIF output'
            print(f"[ERROR] Hadolint parser: {str(e)}", file=sys.stderr)

        return stats

    def parse_shellcheck(self, filepath: str) -> Dict[str, Any]:
        """Parse ShellCheck JSON output"""
        stats = {'tool': 'ShellCheck', 'findings': 0, 'status': '✅ PASS'}

        if not Path(filepath).exists():
            stats['status'] = '⏭️ SKIPPED'
            return stats

        try:
            with open(filepath) as f:
                data = json.load(f)

            # ShellCheck outputs either a flat list (legacy) or {comments: [...]} (json1).
            # Support both formats robustly.
            if isinstance(data, list):
                findings_iter = data
            elif isinstance(data, dict):
                # Check for json1 format with 'comments' key, or fall back to iterating all values
                if 'comments' in data:
                    findings_iter = data['comments']
                else:
                    findings_iter = [item for v in data.values() if isinstance(v, list) for item in v]
            else:
                findings_iter = []

            for finding in findings_iter:
                level = finding.get('level', 'info')
                severity_map = {
                    'error': 'high',
                    'warning': 'medium',
                    'info': 'low',
                    'style': 'info'
                }
                severity = severity_map.get(level, 'low')

                finding_dict = {
                    'tool': 'ShellCheck',
                    'type': 'Shell Script Issue',
                    'severity': severity.upper(),
                    'file': finding.get('file', 'unknown'),
                    'line': finding.get('line', '?'),
                    'code': finding.get('code'),  # For baseline matching
                    'rule': f"SC{finding.get('code', '????')}",
                    'description': finding.get('message', 'No description'),
                    'remediation': 'Follow ShellCheck recommendations for safe shell scripting'
                }

                # Check if this finding is acknowledged in baseline
                if not self._is_acknowledged('shellcheck', finding_dict):
                    self.findings[severity].append(finding_dict)
                    stats['findings'] += 1
                else:
                    self.baseline_counts['shellcheck'] += 1

            if stats['findings'] > 0:
                stats['status'] = '❌ FINDINGS'

        except Exception as e:
            stats['status'] = '⚠️ ERROR: Failed to parse ShellCheck output'
            print(f"[ERROR] ShellCheck parser: {str(e)}", file=sys.stderr)

        return stats

    def parse_kubelinter(self, filepath: str) -> Dict[str, Any]:
        """Parse kube-linter JSON output

        kube-linter v0.7.6+ JSON format:
        {
          "Reports": [
            {
              "Object": {
                "Namespace": "...",
                "Name": "...",
                "GroupVersionKind": {...}
              },
              "Check": "check-name",
              "Diagnostic": {
                "Message": "...",
                "Description": "..."
              }
            }
          ]
        }
        """
        stats = {'tool': 'kube-linter', 'findings': 0, 'status': '✅ PASS'}

        if not Path(filepath).exists():
            stats['status'] = '⏭️ SKIPPED'
            return stats

        try:
            with open(filepath) as f:
                content = f.read().strip()
            if not content:
                return stats
            data = json.loads(content)

            reports = data.get('Reports', [])
            if not reports:
                return stats

            # Deduplicate findings by check:object:message combination
            seen = set()
            unique_findings = []

            for report in reports:
                check_name = report.get('Check', 'unknown')
                diagnostic = report.get('Diagnostic', {})
                message = diagnostic.get('Message', 'kube-linter finding')
                description = diagnostic.get('Description', '')

                # Extract object information
                # kube-linter v0.7.6+ structure has K8sObjectInfo fields under Object.K8sObject
                obj = report.get('Object', {}).get('K8sObject', {})
                namespace = obj.get('Namespace', '')
                name = obj.get('Name', 'unknown')
                gvk = obj.get('GroupVersionKind', {})
                kind = gvk.get('Kind', 'unknown')

                # Construct object identifier
                if namespace:
                    object_id = f"{kind}/{namespace}/{name}"
                else:
                    object_id = f"{kind}/{name}"

                # Deduplication key
                dedup_key = f"{check_name}:{object_id}:{message}"
                if dedup_key in seen:
                    continue
                seen.add(dedup_key)

                # Map check severity (kube-linter doesn't provide severity in JSON)
                # Critical: cluster-admin, privileged containers, host access
                # Critical: Active privilege escalation, container escape, cluster-admin access
                # High: RBAC wildcards, secret access, sensitive mounts
                # Medium: Security best practices (missing configs, hardening)
                # Low: Image tags, general best practices
                critical_checks = {
                    'cluster-admin-role-binding', 'privileged-container',
                    'host-network', 'host-pid', 'host-ipc', 'docker-sock',
                    'access-to-create-pods', 'privilege-escalation-container'
                }
                high_checks = {
                    'access-to-secrets', 'wildcard-in-rules', 'sensitive-host-mounts',
                    'writable-host-mount', 'unsafe-proc-mount', 'unsafe-sysctls',
                    'default-service-account', 'env-var-secret', 'read-secret-from-env-var',
                    'drop-net-raw-capability', 'exposed-services', 'non-isolated-pod',
                    'ssh-port', 'latest-tag', 'no-system-group-binding'
                }
                medium_checks = {
                    'no-liveness-probe', 'no-readiness-probe',
                    'unset-cpu-requirements', 'unset-memory-requirements',
                    'use-namespace', 'non-existent-service-account',
                    'run-as-non-root', 'no-read-only-root-fs', 'privileged-ports'
                }

                if check_name in critical_checks:
                    severity = 'CRITICAL'
                    severity_bucket = 'critical'
                elif check_name in high_checks:
                    severity = 'HIGH'
                    severity_bucket = 'high'
                elif check_name in medium_checks:
                    severity = 'MEDIUM'
                    severity_bucket = 'medium'
                else:
                    severity = 'LOW'
                    severity_bucket = 'low'

                finding = {
                    'tool': 'kube-linter',
                    'type': 'Kubernetes Manifest Security',
                    'severity': severity,
                    'file': object_id,  # Use object ID as "file" for display
                    'line': check_name,  # Use check name as "line" for display
                    'check': check_name,  # For baseline matching (matches acknowledge-findings.py)
                    'rule': check_name,  # For report display (expected by report template)
                    'description': f"{message} (Object: {object_id})",
                    'remediation': description or 'Fix Kubernetes manifest according to check requirements',
                    # For baseline matching
                    'object_kind': kind,
                    'object_name': name,
                    'object_namespace': namespace or None
                }

                # Check if this finding is acknowledged in baseline
                if not self._is_acknowledged('kube-linter', finding):
                    unique_findings.append(finding)
                    self.findings[severity_bucket].append(finding)
                else:
                    self.baseline_counts['kube-linter'] += 1

            stats['findings'] = len(unique_findings)
            if stats['findings'] > 0:
                stats['status'] = '❌ FINDINGS'

        except Exception as e:
            stats['status'] = '⚠️ ERROR: Failed to parse kube-linter JSON'
            print(f"[ERROR] kube-linter parser: {str(e)}", file=sys.stderr)

        return stats

    def parse_rbac_analyzer(self, filepath: str) -> Dict[str, Any]:
        """Parse RBAC Analyzer text output"""
        stats = {'tool': 'RBAC Analyzer', 'findings': 0, 'status': '✅ PASS', 'content': ''}

        if not Path(filepath).exists():
            stats['status'] = '⏭️ SKIPPED'
            return stats

        try:
            with open(filepath) as f:
                content = f.read()
                stats['content'] = content

                # Parse individual RBAC findings from markdown (not just aggregate counts)
                # This enables baseline filtering like all other security tools
                severity_pattern = r'### (CRITICAL|HIGH|WARNING|INFO) \((\d+) findings?\)'
                finding_pattern = r'\d+\. \*\*(.+?)\*\*\s*\n\s*- File: `(.+?)`\s*\n\s*- Issue: (.+?)\n\s*- Fix: (.+?)(?=\n\n|\n\d+\.|\Z)'

                # Split by severity sections
                sections = re.split(severity_pattern, content)

                critical_count = 0
                high_count = 0
                warning_count = 0
                info_count = 0
                findings_count = 0

                for i in range(1, len(sections), 3):
                    severity = sections[i]
                    section_content = sections[i+2] if i+2 < len(sections) else ''

                    # Extract individual findings from this severity section
                    for match in re.finditer(finding_pattern, section_content, re.DOTALL):
                        title = match.group(1).strip()
                        file_path = match.group(2).strip()
                        issue = match.group(3).strip()
                        fix = match.group(4).strip()

                        # Map severity to finding bucket
                        if severity == 'CRITICAL':
                            severity_bucket = 'critical'
                        elif severity == 'HIGH':
                            severity_bucket = 'high'
                        elif severity == 'WARNING':
                            severity_bucket = 'medium'
                        else:  # INFO
                            severity_bucket = 'info'

                        finding_dict = {
                            'tool': 'RBAC Analyzer',
                            'type': 'RBAC Privilege Chain',
                            'severity': severity,
                            'title': title,  # Required for baseline matching
                            'file': file_path,  # Required for baseline matching
                            'line': '?',
                            'rule': f'RBAC_ANALYZER_{severity}',
                            'description': issue,
                            'remediation': fix
                        }

                        # Check if this finding is acknowledged in baseline
                        if not self._is_acknowledged('rbac-analyzer', finding_dict):
                            self.findings[severity_bucket].append(finding_dict)
                            findings_count += 1

                            # Track counts for breakdown
                            if severity == 'CRITICAL':
                                critical_count += 1
                            elif severity == 'HIGH':
                                high_count += 1
                            elif severity == 'WARNING':
                                warning_count += 1
                            else:  # INFO
                                info_count += 1
                        else:
                            self.baseline_counts['rbac-analyzer'] += 1

                stats['findings'] = findings_count

                if stats['findings'] > 0:
                    stats['status'] = '❌ FINDINGS'
                    stats['breakdown'] = {
                        'critical': critical_count,
                        'high': high_count,
                        'warning': warning_count,
                        'info': info_count
                    }

        except Exception as e:
            stats['status'] = '⚠️ ERROR: Failed to parse RBAC analyzer output'
            print(f"[ERROR] RBAC analyzer parser: {str(e)}", file=sys.stderr)

        return stats

    def parse_yamllint(self, filepath: str, max_findings: int = 50) -> Dict[str, Any]:
        """Parse yamllint parsable format output

        Args:
            filepath: Path to yamllint parsable output (file:line:col: [level] message (rule))
            max_findings: Maximum number of findings to include in report (default: 50)

        Format: file:line:column: [level] message (rule)
        Example: ./config/rbac/role.yaml:10:5: [error] line too long (120 > 80 characters) (line-length)
        """
        stats = {'tool': 'yamllint', 'findings': 0, 'status': '✅ PASS', 'findings_data': []}

        if not Path(filepath).exists():
            stats['status'] = '⏭️ SKIPPED'
            return stats

        try:
            with open(filepath) as f:
                lines = f.readlines()

            # Parse each line in parsable format
            # Pattern: filepath:line:column: [level] message (rule)
            pattern = r'^(.+?):(\d+):(\d+): \[(error|warning)\] (.+?) \(([^)]+)\)$'

            for line in lines:
                line = line.strip()
                if not line:
                    continue

                match = re.match(pattern, line)
                if not match:
                    # Skip lines that don't match expected format
                    continue

                file_path, line_num, col, level, message, rule = match.groups()

                finding_dict = {
                    'tool': 'yamllint',
                    'type': 'YAML Issue',
                    'level': level,
                    'file': file_path,
                    'line': int(line_num),
                    'rule': rule,
                    'description': message,
                }

                # Check if this finding is acknowledged in baseline
                if not self._is_acknowledged('yamllint', finding_dict):
                    # Store yamllint findings in separate list (not in main findings dict)
                    # This prevents them from cluttering the security report
                    stats['findings_data'].append(finding_dict)
                    stats['findings'] += 1
                else:
                    self.baseline_counts['yamllint'] += 1

            # Store all findings for dedicated report, but track truncation for comprehensive report
            stats['findings_data_all'] = stats['findings_data'].copy()  # Keep all for dedicated report
            if len(stats['findings_data']) > max_findings:
                stats['findings_data'] = stats['findings_data'][:max_findings]  # Limit for comprehensive report
                stats['truncated'] = True
                stats['total_findings'] = stats['findings']
            else:
                stats['truncated'] = False

            if stats['findings'] > 0:
                stats['status'] = '❌ FINDINGS'

        except Exception as e:
            stats['status'] = '⚠️ ERROR: Failed to parse yamllint output'
            print(f"[ERROR] yamllint parser: {str(e)}", file=sys.stderr)

        return stats

    def parse_actionlint(self, filepath: str) -> Dict[str, Any]:
        """Parse actionlint text output

        Format: <file>:<line>:<col>: <message> [<rule>]
        Example: .github/workflows/test.yml:10:5: invalid expression syntax [expression]
        """
        stats = {'tool': 'actionlint', 'findings': 0, 'status': '✅ PASS', 'findings_data': []}

        if not Path(filepath).exists():
            stats['status'] = '⏭️ SKIPPED'
            return stats

        try:
            with open(filepath) as f:
                content = f.read()

            # Pattern: filepath:line:col: message [rule]
            # actionlint uses this format for all findings
            pattern = r'^(.+?):(\d+):(\d+):\s+(.+?)(?:\s+\[(.+?)\])?$'

            # Regex to strip ANSI color codes (e.g., \x1b[31m for red, \x1b[0m for reset)
            ansi_escape = re.compile(r'\x1b\[[0-9;]*m')

            for line in content.splitlines():
                if not line.strip():
                    continue

                # Strip ANSI color codes before pattern matching
                clean_line = ansi_escape.sub('', line)

                match = re.match(pattern, clean_line)
                if not match:
                    continue

                file_path, line_num, col, message, rule = match.groups()

                # Map severity based on message content
                # GitHub Actions security issues are generally MEDIUM (workflow errors can break CI/CD)
                severity = 'MEDIUM'
                severity_bucket = 'medium'

                # Upgrade to HIGH for security-related issues
                if any(keyword in message.lower() for keyword in ['permission', 'token', 'secret', 'credential']):
                    severity = 'HIGH'
                    severity_bucket = 'high'

                finding = {
                    'tool': 'actionlint',
                    'type': 'GitHub Actions Workflow Issue',
                    'severity': severity,
                    'file': file_path,
                    'line': int(line_num),
                    'rule': rule or 'workflow-syntax',
                    'message': message,  # Use 'message' to match acknowledge-findings.py
                    'description': message,
                    'remediation': 'Fix GitHub Actions workflow syntax according to actionlint recommendation'
                }

                # Check if this finding is acknowledged in baseline
                if not self._is_acknowledged('actionlint', finding):
                    stats['findings_data'].append(finding)
                    self.findings[severity_bucket].append(finding)
                    stats['findings'] += 1
                else:
                    self.baseline_counts['actionlint'] += 1

            if stats['findings'] > 0:
                stats['status'] = '❌ FINDINGS'

        except Exception as e:
            stats['status'] = '⚠️ ERROR: Failed to parse actionlint output'
            print(f"[ERROR] actionlint parser: {str(e)}", file=sys.stderr)

        return stats

    def _get_semgrep_remediation(self, rule_id: str) -> str:
        """Get remediation guidance for Semgrep rules"""
        remediations = {
            'hardcoded-secret-generic': 'Remove hardcoded secret, use environment variables or secret manager',
            'rbac-wildcard-resources': 'Replace wildcard with specific resources following least privilege',
            'rbac-wildcard-verbs': 'Replace wildcard with specific verbs needed for operation',
            'rbac-dangerous-verbs': 'Remove dangerous verbs (escalate/impersonate/bind) or justify usage',
            'insecure-tls-skip-verify': 'Remove InsecureSkipVerify, properly configure certificate validation',
            'weak-crypto-md5': 'Replace MD5 with SHA-256 or stronger hash function',
            'weak-crypto-sha1': 'Replace SHA-1 with SHA-256 or stronger hash function',
            'operator-privileged-pod': 'Remove privileged: true, use specific capabilities if needed',
        }
        return remediations.get(rule_id, 'Follow security best practices for this finding')

    def _calculate_risk_score(self, finding: Dict[str, Any]) -> int:
        """Calculate risk score (0-100) based on severity, exploitability, blast radius, and verification

        Formula: (Severity × Exploitability × Blast Radius × Verification) / 180 * 100
        - Severity: Critical=10, High=7, Medium=4, Low=2
        - Exploitability: Easy=3, Medium=2, Hard=1
        - Blast Radius: Wide=3, Medium=2, Narrow=1
        - Verification: Verified=2, Unverified=1
        """
        # Severity score
        severity_scores = {
            'CRITICAL': 10,
            'HIGH': 7,
            'MEDIUM': 4,
            'LOW': 2,
            'INFO': 1
        }
        severity_score = severity_scores.get(finding.get('severity', 'LOW'), 2)

        # Exploitability score (based on finding type and tool)
        tool = finding.get('tool', '')
        rule = finding.get('rule', '').lower()

        # Critical secrets are easy to exploit (especially if repo is public)
        if tool in ['Gitleaks', 'TruffleHog']:
            exploitability = 3  # Easy - just copy/paste the secret
        # SQL/Command injection are easy to exploit
        elif 'sql' in rule or 'injection' in rule or 'command' in rule:
            exploitability = 3  # Easy - send malicious input
        # RBAC/privilege escalation requires cluster access
        elif 'rbac' in rule or 'wildcard' in rule or 'privilege' in rule:
            exploitability = 2  # Medium - need initial access
        # TLS/crypto issues require man-in-the-middle
        elif 'tls' in rule or 'ssl' in rule or 'crypto' in rule:
            exploitability = 2  # Medium - need network position
        # Container security issues (privileged, host-network)
        elif 'privileged' in rule or 'host-network' in rule or 'host-pid' in rule:
            exploitability = 2  # Medium - need pod deployment access
        # Most infrastructure issues are moderately exploitable
        elif tool in ['kube-linter', 'Hadolint', 'Semgrep']:
            exploitability = 2  # Medium
        # Code quality and configuration issues are harder to exploit
        else:
            exploitability = 1  # Hard - requires specific conditions

        # Blast radius (impact scope)
        # Verified secrets and cluster-admin have widest blast radius
        if tool == 'TruffleHog' or 'cluster-admin' in rule:
            blast_radius = 3  # Wide - full system/account access
        # Hardcoded secrets can access entire services
        elif tool == 'Gitleaks':
            blast_radius = 3  # Wide - depends on secret scope
        # SQL/Command injection can compromise entire application
        elif 'sql' in rule or 'injection' in rule or 'command' in rule:
            blast_radius = 3  # Wide - full application compromise
        # RBAC wildcards and privilege escalation
        elif tool == 'RBAC Analyzer' or 'wildcard' in rule or 'privilege' in rule:
            blast_radius = 3  # Wide - can escalate to full cluster access
        # Container escape (privileged, host access)
        elif 'privileged' in rule or 'host-network' in rule or 'host-pid' in rule:
            blast_radius = 2  # Medium - node access, potential cluster spread
        # Infrastructure misconfigurations
        elif tool in ['kube-linter', 'Hadolint']:
            blast_radius = 2  # Medium - workload or container scope
        # Configuration and code quality issues
        else:
            blast_radius = 1  # Narrow - limited impact

        # Verification status (TruffleHog verified = 2, everything else = 1)
        verification = 2 if tool == 'TruffleHog' else 1

        # Calculate final score (0-100 scale)
        raw_score = severity_score * exploitability * blast_radius * verification
        # Max possible: 10 * 3 * 3 * 2 = 180
        normalized_score = min(100, int((raw_score / 180.0) * 100))

        return normalized_score

    def _generate_attack_scenario(self, finding: Dict[str, Any]) -> Dict[str, Any]:
        """Generate attack scenario for critical/high findings

        Returns dict with:
        - attack_steps: List of exploitation steps
        - prerequisites: What attacker needs
        - impact: Business/technical impact
        - exploitability: EASY/MEDIUM/HARD
        """
        tool = finding.get('tool', '')
        rule = finding.get('rule', '')
        severity = finding.get('severity', '')

        scenario = {
            'attack_steps': [],
            'prerequisites': [],
            'impact': [],
            'exploitability': 'MEDIUM'
        }

        # Gitleaks - Hardcoded secrets
        if tool == 'Gitleaks':
            scenario['attack_steps'] = [
                f"Attacker gains read access to repository (public repo or compromised credentials)",
                f"Extracts hardcoded secret from {finding.get('file', 'file')}:{finding.get('line', '?')}",
                "Uses secret to authenticate to target system",
                "Escalates privileges or exfiltrates data depending on secret scope"
            ]
            scenario['prerequisites'] = [
                "Repository read access (public repo = anyone)",
                "OR: Git history access (secret may persist in history)"
            ]
            scenario['impact'] = [
                "Unauthorized access to systems/services",
                "Data exfiltration or modification",
                "Compliance violation (secrets in version control)",
                "Reputation damage if publicly exposed"
            ]
            scenario['exploitability'] = 'EASY'

        # TruffleHog - Verified credentials
        elif tool == 'TruffleHog':
            detector = finding.get('detector', 'Unknown service')
            scenario['attack_steps'] = [
                f"Attacker gains read access to repository",
                f"Extracts VERIFIED {detector} credential from {finding.get('file', 'file')}:{finding.get('line', '?')}",
                f"Immediately uses credential (already verified as ACTIVE)",
                f"Full access to {detector} account/resources",
                "Lateral movement to connected systems"
            ]
            scenario['prerequisites'] = [
                "Repository read access",
                "Credential is VERIFIED (actively working)"
            ]
            scenario['impact'] = [
                f"CRITICAL: Full access to {detector} account",
                "Data breach of all accessible resources",
                "Financial impact (unauthorized resource usage)",
                "Compliance violations (data protection regulations)",
                "Immediate security incident requiring disclosure"
            ]
            scenario['exploitability'] = 'EASY'

        # RBAC Analyzer - Privilege escalation
        elif tool == 'RBAC Analyzer':
            scenario['attack_steps'] = [
                "Attacker compromises a low-privilege service account or pod",
                f"Exploits RBAC chain: {finding.get('title', 'privilege escalation path')}",
                "Escalates to cluster-admin or high-privilege role",
                "Full cluster control achieved"
            ]
            scenario['prerequisites'] = [
                "Initial access to Kubernetes cluster",
                "Compromised pod or service account",
                "RBAC misconfiguration allows privilege escalation"
            ]
            scenario['impact'] = [
                "Full cluster compromise",
                "Access to all secrets and workloads",
                "Data exfiltration across namespaces",
                "Persistent backdoor installation",
                "Cryptocurrency mining or ransomware deployment"
            ]
            scenario['exploitability'] = 'MEDIUM'

        # kube-linter - Kubernetes security issues
        elif tool == 'kube-linter':
            if 'cluster-admin' in rule.lower():
                scenario['attack_steps'] = [
                    "Attacker compromises service account with cluster-admin binding",
                    "Full cluster access immediately granted",
                    "Creates backdoor admin accounts",
                    "Deploys malicious workloads"
                ]
                scenario['exploitability'] = 'EASY'
            elif 'wildcard' in rule.lower():
                scenario['attack_steps'] = [
                    "Attacker compromises service account with wildcard permissions",
                    "Exploits overly broad access to resources",
                    "Accesses secrets or escalates privileges",
                    "Lateral movement across namespaces"
                ]
                scenario['exploitability'] = 'MEDIUM'
            elif 'privileged' in rule.lower():
                scenario['attack_steps'] = [
                    "Attacker gains code execution in privileged container",
                    "Escapes container to host system",
                    "Full node compromise",
                    "Pivots to other nodes in cluster"
                ]
                scenario['exploitability'] = 'MEDIUM'
            else:
                scenario['attack_steps'] = [
                    "Attacker exploits Kubernetes misconfiguration",
                    "Gains elevated access or escapes isolation",
                    "Compromises workload or data"
                ]
                scenario['exploitability'] = 'MEDIUM'

            scenario['prerequisites'] = [
                "Access to Kubernetes cluster",
                "Ability to deploy or modify resources",
                f"Kubernetes object exists: {finding.get('file', 'unknown')}"
            ]
            scenario['impact'] = [
                "Container escape or privilege escalation",
                "Access to node resources",
                "Potential cluster-wide compromise",
                "Data exfiltration"
            ]

        # Semgrep - Code security issues
        elif tool == 'Semgrep':
            if 'sql-injection' in rule.lower():
                scenario['attack_steps'] = [
                    "Attacker sends malicious SQL in user input",
                    "Application executes unvalidated query",
                    "Database compromise via SQL injection",
                    "Data exfiltration or modification"
                ]
                scenario['exploitability'] = 'EASY'
            elif 'command-injection' in rule.lower():
                scenario['attack_steps'] = [
                    "Attacker injects shell commands in user input",
                    "Application executes commands on server",
                    "Remote code execution achieved",
                    "Server takeover"
                ]
                scenario['exploitability'] = 'EASY'
            elif 'tls' in rule.lower() or 'insecure' in rule.lower():
                scenario['attack_steps'] = [
                    "Attacker performs man-in-the-middle attack",
                    "Insecure TLS configuration allows interception",
                    "Sensitive data captured in transit",
                    "Credentials or tokens stolen"
                ]
                scenario['exploitability'] = 'MEDIUM'
            else:
                scenario['attack_steps'] = [
                    "Attacker exploits code vulnerability",
                    "Gains unauthorized access or elevation",
                    "Compromises application security"
                ]
                scenario['exploitability'] = 'MEDIUM'

            scenario['prerequisites'] = [
                "Access to application endpoint",
                "Ability to provide malicious input",
                f"Vulnerable code at {finding.get('file', 'file')}:{finding.get('line', '?')}"
            ]
            scenario['impact'] = [
                "Application compromise",
                "Data breach or corruption",
                "Unauthorized access",
                "Potential system takeover"
            ]

        # Default scenario for other tools
        else:
            scenario['attack_steps'] = [
                "Attacker identifies security misconfiguration",
                "Exploits weakness to gain unauthorized access",
                "Compromises security posture"
            ]
            scenario['prerequisites'] = [
                f"Access to affected system/component",
                f"Vulnerability at {finding.get('file', 'file')}:{finding.get('line', '?')}"
            ]
            scenario['impact'] = [
                "Security control bypass",
                "Potential unauthorized access",
                "Weakened security posture"
            ]
            scenario['exploitability'] = 'MEDIUM'

        return scenario

    def _map_compliance_frameworks(self, finding: Dict[str, Any]) -> List[Dict[str, str]]:
        """Map finding to compliance framework controls

        Frameworks selected for infrastructure/platform security:
        - SOC2: Service Organization Control 2 (Trust Services Criteria for cloud platforms)
        - ISO27001: Information Security Management System (international standard)

        Note: GDPR/PCI-DSS removed as they're not applicable to infrastructure platform code.
        GDPR is for personal data protection (relevant for applications built on RHOAI, not RHOAI itself).
        PCI-DSS is for payment card processing (not applicable to AI/ML platform).

        Returns list of dicts with:
        - framework: SOC2, ISO27001
        - control: Specific control number/name
        - description: What the control requires
        """
        tool = finding.get('tool', '')
        rule = finding.get('rule', '').lower()
        mappings = []

        # Hardcoded secrets (Gitleaks, TruffleHog)
        if tool in ['Gitleaks', 'TruffleHog']:
            mappings.extend([
                {
                    'framework': 'SOC2',
                    'control': 'CC6.1',
                    'description': 'Logical and physical access controls restrict access to sensitive information'
                },
                {
                    'framework': 'ISO27001',
                    'control': 'A.9.4.1',
                    'description': 'Information access restriction - credentials must not be stored in code'
                }
            ])

        # RBAC issues
        if tool == 'RBAC Analyzer' or 'rbac' in rule or 'wildcard' in rule:
            mappings.extend([
                {
                    'framework': 'SOC2',
                    'control': 'CC6.2',
                    'description': 'Prior to issuing system credentials, the entity registers and authorizes new users'
                },
                {
                    'framework': 'ISO27001',
                    'control': 'A.9.2.3',
                    'description': 'Management of privileged access rights'
                }
            ])

        # Privileged containers, insecure configurations
        if 'privileged' in rule or 'host-network' in rule or 'root' in rule:
            mappings.extend([
                {
                    'framework': 'SOC2',
                    'control': 'CC6.6',
                    'description': 'Logical and physical access controls restrict access'
                },
                {
                    'framework': 'ISO27001',
                    'control': 'A.12.6.1',
                    'description': 'Management of technical vulnerabilities'
                }
            ])

        # TLS/encryption issues
        if 'tls' in rule or 'insecure' in rule or 'crypto' in rule:
            mappings.extend([
                {
                    'framework': 'SOC2',
                    'control': 'CC6.7',
                    'description': 'Entity transmits and stores data using encryption'
                },
                {
                    'framework': 'ISO27001',
                    'control': 'A.10.1.1',
                    'description': 'Policy on the use of cryptographic controls'
                }
            ])

        # If no specific mappings, add generic security control mapping
        if not mappings and finding.get('severity') in ['CRITICAL', 'HIGH']:
            mappings.append({
                'framework': 'SOC2',
                'control': 'CC6.1',
                'description': 'Logical and physical access controls'
            })
            mappings.append({
                'framework': 'ISO27001',
                'control': 'A.12.6.1',
                'description': 'Management of technical vulnerabilities'
            })

        return mappings

    def _map_owasp_cwe(self, finding: Dict[str, Any]) -> Dict[str, Any]:
        """Map finding to OWASP Top 10 and CWE

        Returns dict with:
        - owasp: OWASP Top 10 2021 category
        - cwe: CWE number and description
        - cvss: CVSS 3.1 score (if applicable)
        """
        tool = finding.get('tool', '')
        rule = finding.get('rule', '').lower()
        severity = finding.get('severity', '')

        mapping = {
            'owasp': None,
            'cwe': None,
            'cvss': None
        }

        # Hardcoded credentials
        if tool in ['Gitleaks', 'TruffleHog']:
            mapping['owasp'] = 'A07:2021 - Identification and Authentication Failures'
            mapping['cwe'] = 'CWE-798: Use of Hard-coded Credentials'
            if severity == 'CRITICAL':
                mapping['cvss'] = '9.8 (Critical) - AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H'

        # SQL Injection
        elif 'sql-injection' in rule or 'sql' in rule:
            mapping['owasp'] = 'A03:2021 - Injection'
            mapping['cwe'] = 'CWE-89: SQL Injection'
            mapping['cvss'] = '9.8 (Critical) - AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H'

        # Command Injection
        elif 'command-injection' in rule or 'shell-injection' in rule:
            mapping['owasp'] = 'A03:2021 - Injection'
            mapping['cwe'] = 'CWE-78: OS Command Injection'
            mapping['cvss'] = '9.8 (Critical) - AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H'

        # Insecure TLS
        elif 'tls' in rule or 'ssl' in rule or 'insecure' in rule:
            mapping['owasp'] = 'A02:2021 - Cryptographic Failures'
            mapping['cwe'] = 'CWE-295: Improper Certificate Validation'
            mapping['cvss'] = '7.4 (High) - AV:N/AC:H/PR:N/UI:N/S:U/C:H/I:H/A:N'

        # Weak Cryptography
        elif 'crypto' in rule or 'md5' in rule or 'sha1' in rule:
            mapping['owasp'] = 'A02:2021 - Cryptographic Failures'
            mapping['cwe'] = 'CWE-327: Use of a Broken or Risky Cryptographic Algorithm'
            mapping['cvss'] = '7.5 (High) - AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:N/A:N'

        # Access Control (RBAC wildcards, privilege escalation)
        elif 'rbac' in rule or 'wildcard' in rule or 'privilege' in rule or tool == 'RBAC Analyzer':
            mapping['owasp'] = 'A01:2021 - Broken Access Control'
            mapping['cwe'] = 'CWE-269: Improper Privilege Management'
            if severity == 'CRITICAL':
                mapping['cvss'] = '8.8 (High) - AV:N/AC:L/PR:L/UI:N/S:U/C:H/I:H/A:H'
            else:
                mapping['cvss'] = '6.5 (Medium) - AV:N/AC:L/PR:L/UI:N/S:U/C:H/I:N/A:N'

        # Security Misconfiguration
        elif tool in ['kube-linter', 'Hadolint'] or 'config' in rule:
            mapping['owasp'] = 'A05:2021 - Security Misconfiguration'
            mapping['cwe'] = 'CWE-16: Configuration'

        # Default for unmatched
        else:
            mapping['owasp'] = 'A05:2021 - Security Misconfiguration'
            mapping['cwe'] = 'CWE-1004: Sensitive Cookie Without HttpOnly Flag'

        return mapping

    def generate_report(self, output_file: str, json_summary_file: Optional[str] = None, yamllint_report_file: Optional[str] = None):
        """Generate comprehensive markdown security report, optional JSON summary, and optional yamllint report"""

        # Load baseline to filter acknowledged findings
        self.load_baseline()

        # Parse all tool outputs
        self.tool_stats['gitleaks'] = self.parse_gitleaks(f'{self.workspace}/gitleaks.json')
        self.tool_stats['trufflehog'] = self.parse_trufflehog(f'{self.workspace}/trufflehog.json')
        self.tool_stats['semgrep'] = self.parse_semgrep_sarif(f'{self.workspace}/semgrep.sarif')
        self.tool_stats['hadolint'] = self.parse_hadolint_sarif(f'{self.workspace}/hadolint.sarif')
        self.tool_stats['shellcheck'] = self.parse_shellcheck(f'{self.workspace}/shellcheck.json')
        self.tool_stats['yamllint'] = self.parse_yamllint(f'{self.workspace}/yamllint.txt', max_findings=self.yamllint_limit)
        self.tool_stats['actionlint'] = self.parse_actionlint(f'{self.workspace}/actionlint.txt')
        self.tool_stats['kube-linter'] = self.parse_kubelinter(f'{self.workspace}/kube-linter.json')
        self.tool_stats['rbac'] = self.parse_rbac_analyzer(f'{self.workspace}/rbac-analysis.md')

        # Calculate risk scores for all findings
        for severity in ['critical', 'high', 'medium', 'low', 'info']:
            for finding in self.findings[severity]:
                finding['risk_score'] = self._calculate_risk_score(finding)

        # Calculate totals
        total_findings = sum(len(findings) for findings in self.findings.values())
        critical_count = len(self.findings['critical'])
        high_count = len(self.findings['high'])
        medium_count = len(self.findings['medium'])
        low_count = len(self.findings['low'])

        # Determine overall security posture
        if critical_count > 0:
            posture = 'CRITICAL'
            posture_desc = 'Immediate action required - critical vulnerabilities detected'
        elif high_count > 0:
            posture = 'HIGH'
            posture_desc = 'High-severity issues detected - prompt review needed'
        elif medium_count > 0:
            posture = 'MEDIUM'
            posture_desc = 'Medium-severity issues detected - review recommended'
        elif low_count > 0:
            posture = 'LOW'
            posture_desc = 'Low-severity issues detected - minor improvements suggested'
        else:
            posture = 'CLEAN'
            posture_desc = 'No security issues detected'

        # Generate report
        try:
            with open(output_file, 'w') as f:
                # Header
                f.write(f"# Comprehensive Security Scan Report\n\n")
                f.write(f"**Generated:** {datetime.now(timezone.utc).strftime('%Y-%m-%d %H:%M:%S')} UTC\n\n")
                f.write(f"**Repository:** {self.github.get('repository', 'unknown')}\n\n")
                f.write(f"**Commit:** {self.github.get('sha', 'unknown')}\n\n")
                f.write(f"**Branch:** {self.github.get('ref_name', 'unknown')}\n\n")
                f.write(f"**Workflow Run:** {self.github.get('run_url', 'N/A')}\n\n")
                f.write(f"---\n\n")

                # Executive Summary
                f.write(f"## Executive Summary\n\n")
                f.write(f"**Security Posture:** {posture}\n\n")
                f.write(f"{posture_desc}\n\n")
                f.write(f"**Total Findings:** {total_findings}\n\n")
                f.write(f"- Critical: {critical_count}\n")
                f.write(f"- High: {high_count}\n")
                f.write(f"- Medium: {medium_count}\n")
                f.write(f"- Low: {low_count}\n")
                f.write(f"- Info: {len(self.findings['info'])}\n\n")

                # Add baseline acknowledgments note
                # Use baseline_counts (actual filtered findings) instead of baseline (file entries)
                total_acknowledged = sum(self.baseline_counts.values())
                if total_acknowledged > 0:
                    f.write(f"**Acknowledged Findings:** {total_acknowledged} findings have been acknowledged ")
                    f.write(f"as false positives or accepted risks (see `.github/config/security-baseline.yaml`)\n\n")
                    # Show breakdown by tool (using actual filtered counts)
                    acknowledged_by_tool = []
                    for tool, count in self.baseline_counts.items():
                        if count > 0:
                            tool_display = {
                                'kube-linter': 'kube-linter',
                                'rbac-analyzer': 'RBAC Analyzer'
                            }.get(tool, tool.capitalize())
                            acknowledged_by_tool.append(f"{tool_display}: {count}")
                    if acknowledged_by_tool:
                        f.write(f"- {', '.join(acknowledged_by_tool)}\n\n")

                f.write(f"---\n\n")

                # Tool Status Table
                f.write(f"## Scan Tool Status\n\n")
                f.write(f"| Tool | Purpose | Status | Findings |\n")
                f.write(f"|------|---------|--------|----------|\n")
                f.write(f"| {self.tool_stats['gitleaks']['tool']} | Pattern-based secret detection | {self.tool_stats['gitleaks']['status']} | {self.tool_stats['gitleaks']['findings']} |\n")
                f.write(f"| {self.tool_stats['trufflehog']['tool']} | Verified credential detection (800+ types) | {self.tool_stats['trufflehog']['status']} | {self.tool_stats['trufflehog']['findings']} |\n")
                f.write(f"| {self.tool_stats['semgrep']['tool']} | Custom security rules (27 operator-focused) | {self.tool_stats['semgrep']['status']} | {self.tool_stats['semgrep']['findings']} |\n")
                f.write(f"| {self.tool_stats['hadolint']['tool']} | Dockerfile best practices | {self.tool_stats['hadolint']['status']} | {self.tool_stats['hadolint']['findings']} |\n")
                f.write(f"| {self.tool_stats['shellcheck']['tool']} | Shell script security | {self.tool_stats['shellcheck']['status']} | {self.tool_stats['shellcheck']['findings']} |\n")
                f.write(f"| {self.tool_stats['yamllint']['tool']} | YAML syntax and style validation | {self.tool_stats['yamllint']['status']} | {self.tool_stats['yamllint']['findings']} |\n")
                f.write(f"| {self.tool_stats['actionlint']['tool']} | GitHub Actions workflow validation | {self.tool_stats['actionlint']['status']} | {self.tool_stats['actionlint']['findings']} |\n")
                f.write(f"| {self.tool_stats['kube-linter']['tool']} | Kubernetes manifest security | {self.tool_stats['kube-linter']['status']} | {self.tool_stats['kube-linter']['findings']} |\n")
                f.write(f"| {self.tool_stats['rbac']['tool']} | RBAC privilege chain analysis | {self.tool_stats['rbac']['status']} | {self.tool_stats['rbac']['findings']} |\n\n")
                f.write(f"---\n\n")

                # Quick Wins section (high impact, low effort fixes)
                if critical_count > 0 or high_count > 0:
                    f.write(f"## 🎯 Quick Wins (High Impact, Low Effort)\n\n")
                    f.write(f"These findings are relatively easy to fix but provide significant security improvements:\n\n")

                    # Collect all critical/high findings with risk scores
                    quick_win_candidates = []
                    for finding in self.findings['critical'] + self.findings['high']:
                        # Determine effort level based on finding type
                        effort = 'MEDIUM'
                        tool = finding.get('tool', '')
                        rule = finding.get('rule', '').lower()

                        # Low effort fixes
                        if tool in ['Gitleaks', 'TruffleHog']:
                            effort = 'LOW'  # Remove hardcoded secret = 5-10 min
                        elif 'privileged' in rule or 'root' in rule:
                            effort = 'LOW'  # Change Dockerfile/manifest = 5 min
                        elif tool == 'Hadolint':
                            effort = 'LOW'  # Fix Dockerfile best practice = 2-5 min
                        elif 'wildcard' in rule and tool in ['kube-linter', 'Semgrep']:
                            effort = 'MEDIUM'  # Replace wildcard = 15-30 min
                        else:
                            effort = 'MEDIUM'

                        # Quick wins are high risk score + low effort
                        if effort == 'LOW' and finding.get('risk_score', 0) >= 60:
                            quick_win_candidates.append({
                                'finding': finding,
                                'effort': effort,
                                'risk_score': finding.get('risk_score', 0)
                            })

                    # Sort by risk score (highest first)
                    quick_win_candidates.sort(key=lambda x: x['risk_score'], reverse=True)

                    # Show top 5 quick wins
                    if quick_win_candidates:
                        for i, qw in enumerate(quick_win_candidates[:5], 1):
                            finding = qw['finding']
                            f.write(f"{i}. **[Risk: {finding.get('risk_score', 0)}/100]** ")
                            f.write(f"{finding['type']} in `{finding['file']}:{finding['line']}`\n")
                            f.write(f"   - **Fix:** {finding['remediation']}\n")
                            f.write(f"   - **Impact:** Eliminates {finding['severity'].lower()}-severity vulnerability\n")
                            f.write(f"   - **Effort:** 5-10 minutes\n\n")
                    else:
                        f.write(f"*No quick wins identified. All critical/high findings require moderate effort.*\n\n")

                    f.write(f"---\n\n")

                # Critical Findings (Enhanced with attack scenarios)
                if self.findings['critical']:
                    f.write(f"## ⚠️ Critical Findings ({len(self.findings['critical'])})\n\n")
                    f.write(f"**IMMEDIATE ACTION REQUIRED - These vulnerabilities pose severe security risks**\n\n")
                    for i, finding in enumerate(self.findings['critical'], 1):
                        f.write(f"### Finding #{i}: {finding['type']}\n\n")
                        f.write(f"**Severity:** CRITICAL | **Risk Score:** {finding.get('risk_score', 0)}/100\n\n")
                        f.write(f"**Tool:** {finding['tool']} | **Rule:** {finding['rule']}\n\n")

                        # Location
                        f.write(f"**Location:** `{finding['file']}:{finding['line']}`\n\n")

                        # Description
                        f.write(f"**Description:** {finding['description']}\n\n")

                        # Attack Scenario
                        scenario = self._generate_attack_scenario(finding)
                        f.write(f"#### 🎯 Attack Scenario\n\n")
                        f.write(f"**How an attacker could exploit this:**\n\n")
                        for step_num, step in enumerate(scenario['attack_steps'], 1):
                            f.write(f"{step_num}. {step}\n")
                        f.write(f"\n")

                        f.write(f"**Prerequisites for Exploitation:**\n")
                        for prereq in scenario['prerequisites']:
                            f.write(f"- {prereq}\n")
                        f.write(f"\n")

                        f.write(f"**Potential Impact:**\n")
                        for impact in scenario['impact']:
                            f.write(f"- {impact}\n")
                        f.write(f"\n")

                        f.write(f"**Exploitability:** {scenario['exploitability']}\n\n")

                        # Compliance Impact
                        compliance = self._map_compliance_frameworks(finding)
                        if compliance:
                            f.write(f"#### 📋 Compliance Impact\n\n")
                            for mapping in compliance:
                                f.write(f"- ❌ **{mapping['framework']}**: {mapping['control']} - {mapping['description']}\n")
                            f.write(f"\n")

                        # OWASP/CWE Mapping
                        owasp_cwe = self._map_owasp_cwe(finding)
                        if owasp_cwe['owasp'] or owasp_cwe['cwe']:
                            f.write(f"#### 🔍 Security Classification\n\n")
                            if owasp_cwe['owasp']:
                                f.write(f"- **OWASP Top 10 2021:** {owasp_cwe['owasp']}\n")
                            if owasp_cwe['cwe']:
                                f.write(f"- **CWE:** {owasp_cwe['cwe']}\n")
                            if owasp_cwe['cvss']:
                                f.write(f"- **CVSS Score:** {owasp_cwe['cvss']}\n")
                            f.write(f"\n")

                        # Remediation
                        f.write(f"#### 🔧 Remediation\n\n")
                        f.write(f"**Immediate Action Required:**\n\n")
                        f.write(f"{finding['remediation']}\n\n")

                        f.write(f"---\n\n")

                    f.write(f"\n")

                # High Findings (Enhanced with attack scenarios)
                if self.findings['high']:
                    f.write(f"## 🔴 High-Severity Findings ({len(self.findings['high'])})\n\n")
                    f.write(f"**High priority - Address within 7 days**\n\n")
                    for i, finding in enumerate(self.findings['high'], 1):
                        f.write(f"### Finding #{i}: {finding['type']}\n\n")
                        f.write(f"**Severity:** HIGH | **Risk Score:** {finding.get('risk_score', 0)}/100\n\n")
                        f.write(f"**Tool:** {finding['tool']} | **Rule:** {finding['rule']}\n\n")

                        # Location
                        f.write(f"**Location:** `{finding['file']}:{finding['line']}`\n\n")

                        # Description
                        f.write(f"**Description:** {finding['description']}\n\n")

                        # Attack Scenario (collapsed by default for high findings)
                        scenario = self._generate_attack_scenario(finding)
                        f.write(f"<details>\n")
                        f.write(f"<summary><strong>🎯 Attack Scenario</strong> (click to expand)</summary>\n\n")
                        f.write(f"**How an attacker could exploit this:**\n\n")
                        for step_num, step in enumerate(scenario['attack_steps'], 1):
                            f.write(f"{step_num}. {step}\n")
                        f.write(f"\n")

                        f.write(f"**Prerequisites:** ")
                        f.write(f"{', '.join(scenario['prerequisites'])}\n\n")

                        f.write(f"**Impact:** ")
                        f.write(f"{', '.join(scenario['impact'])}\n\n")

                        f.write(f"**Exploitability:** {scenario['exploitability']}\n\n")
                        f.write(f"</details>\n\n")

                        # Compliance Impact (brief)
                        compliance = self._map_compliance_frameworks(finding)
                        if compliance:
                            f.write(f"**Compliance Impact:** ")
                            frameworks = [f"{m['framework']} {m['control']}" for m in compliance]
                            f.write(f"{', '.join(frameworks)}\n\n")

                        # OWASP/CWE (brief)
                        owasp_cwe = self._map_owasp_cwe(finding)
                        if owasp_cwe['owasp']:
                            f.write(f"**OWASP:** {owasp_cwe['owasp']}\n\n")

                        # Remediation
                        f.write(f"**Remediation:** {finding['remediation']}\n\n")

                        f.write(f"---\n\n")

                    f.write(f"\n")

                # Medium Findings
                if self.findings['medium']:
                    f.write(f"## Medium-Severity Findings ({len(self.findings['medium'])})\n\n")
                    for i, finding in enumerate(self.findings['medium'], 1):
                        f.write(f"### {i}. {finding['type']} ({finding['tool']})\n\n")
                        f.write(f"- **File:** `{finding['file']}:{finding['line']}`\n")
                        f.write(f"- **Rule:** {finding['rule']}\n")
                        f.write(f"- **Description:** {finding['description']}\n")
                        f.write(f"- **Remediation:** {finding['remediation']}\n\n")
                    f.write(f"---\n\n")

                # RBAC Analysis
                if self.tool_stats['rbac']['content']:
                    f.write(f"## RBAC Privilege Chain Analysis\n\n")
                    # RBAC analyzer already outputs markdown format - no code blocks needed
                    f.write(self.tool_stats['rbac']['content'])
                    f.write(f"\n---\n\n")

                # YAML Lint Issues (separate section, non-security)
                yamllint_stats = self.tool_stats.get('yamllint', {})
                if yamllint_stats.get('findings', 0) > 0:
                    f.write(f"## Code Quality: YAML Formatting Issues\n\n")
                    f.write(f"**Note:** These are style/formatting issues, not security vulnerabilities.\n\n")

                    yamllint_findings = yamllint_stats.get('findings_data', [])
                    if yamllint_stats.get('truncated', False):
                        total = yamllint_stats.get('total_findings', len(yamllint_findings))
                        f.write(f"Showing {len(yamllint_findings)} of {total} yamllint findings (truncated for readability).\n\n")

                    # Group by severity
                    errors = [f for f in yamllint_findings if f.get('level') == 'error']
                    warnings = [f for f in yamllint_findings if f.get('level') == 'warning']

                    if errors:
                        f.write(f"<details>\n")
                        f.write(f"<summary>YAML Errors ({len(errors)}) - Click to expand</summary>\n\n")
                        for i, finding in enumerate(errors, 1):
                            f.write(f"{i}. **{finding['rule']}** in `{finding['file']}:{finding['line']}`\n")
                            f.write(f"   - {finding['description']}\n\n")
                        f.write(f"</details>\n\n")

                    if warnings:
                        f.write(f"<details>\n")
                        f.write(f"<summary>YAML Warnings ({len(warnings)}) - Click to expand</summary>\n\n")
                        for i, finding in enumerate(warnings, 1):
                            f.write(f"{i}. **{finding['rule']}** in `{finding['file']}:{finding['line']}`\n")
                            f.write(f"   - {finding['description']}\n\n")
                        f.write(f"</details>\n\n")

                    f.write(f"**Remediation:** These are YAML style and formatting issues, not security vulnerabilities. ")
                    f.write(f"See the dedicated **yamllint-report.md** artifact for complete findings and detailed remediation instructions.\n\n")
                    f.write(f"---\n\n")

                # Recommendations (dynamic based on actual findings)
                f.write(f"## 📋 Recommendations\n\n")

                if critical_count > 0:
                    f.write(f"### Immediate Actions (Critical)\n\n")
                    rec_num = 1

                    # Check for specific tool findings
                    has_gitleaks = any(f['tool'] == 'Gitleaks' for f in self.findings['critical'])
                    has_trufflehog = any(f['tool'] == 'TruffleHog' for f in self.findings['critical'])
                    has_rbac_critical = any(f['tool'] == 'RBAC Analyzer' for f in self.findings['critical'])

                    if has_trufflehog:
                        f.write(f"{rec_num}. **URGENT: Rotate verified credentials immediately** - TruffleHog confirmed these credentials are active\n")
                        rec_num += 1
                    if has_gitleaks:
                        f.write(f"{rec_num}. **Remove hardcoded secrets** from codebase and use secret management\n")
                        rec_num += 1
                    if has_rbac_critical:
                        f.write(f"{rec_num}. **Fix critical RBAC permissions** - remove wildcards and dangerous verbs\n")
                        rec_num += 1
                    f.write("\n")

                if high_count > 0:
                    f.write(f"### High Priority (This Week)\n\n")
                    rec_num = 1

                    has_semgrep_high = any(f['tool'] == 'Semgrep' for f in self.findings['high'])
                    has_shellcheck_high = any(f['tool'] == 'ShellCheck' for f in self.findings['high'])
                    has_rbac_high = any(f['tool'] == 'RBAC Analyzer' for f in self.findings['high'])

                    if has_semgrep_high:
                        f.write(f"{rec_num}. Review and fix high-severity Semgrep findings\n")
                        rec_num += 1
                    if has_shellcheck_high:
                        f.write(f"{rec_num}. Fix high-severity ShellCheck issues to prevent command injection\n")
                        rec_num += 1
                    if has_rbac_high:
                        f.write(f"{rec_num}. Tighten high-risk RBAC permissions\n")
                        rec_num += 1
                    f.write("\n")

                if medium_count > 0 or low_count > 0:
                    f.write(f"### Medium/Low Priority (Next Sprint)\n\n")
                    rec_num = 1

                    has_hadolint = self.tool_stats.get('hadolint', {}).get('findings', 0) > 0
                    has_shellcheck_medium = any(f['tool'] == 'ShellCheck' for f in self.findings['medium'])
                    has_semgrep_medium = any(f['tool'] == 'Semgrep' for f in self.findings['medium'])

                    if has_hadolint:
                        f.write(f"{rec_num}. Address Dockerfile best practice violations\n")
                        rec_num += 1
                    if has_shellcheck_medium:
                        f.write(f"{rec_num}. Fix ShellCheck warnings in scripts\n")
                        rec_num += 1
                    if has_semgrep_medium:
                        f.write(f"{rec_num}. Review medium-severity Semgrep findings\n")
                        rec_num += 1
                    f.write("\n")

                # Next Steps
                f.write(f"## 🎯 Next Steps\n\n")
                f.write(f"1. **Review this report** and triage findings by severity\n")
                f.write(f"2. **Check SARIF results** in the Security tab for detailed code locations\n")
                f.write(f"3. **Download artifacts** from the workflow run for raw tool outputs\n")
                f.write(f"4. **Create JIRA tickets** for remediation work and track progress\n\n")
                f.write(f"---\n\n")
                f.write(f"*This report was automatically generated by the Security Full Codebase Scan workflow.*\n")
        except IOError as e:
            print(f"[ERROR] Failed to write security report to {output_file}: {str(e)}", file=sys.stderr)
            sys.exit(1)

        # Generate JSON summary if requested
        if json_summary_file:
            self._generate_json_summary(json_summary_file, posture, total_findings, critical_count, high_count, medium_count, low_count)

        # Generate dedicated yamllint report if requested
        if yamllint_report_file:
            self._generate_yamllint_report(yamllint_report_file)

    def _generate_json_summary(self, output_file: str, posture: str, total: int, critical: int, high: int, medium: int, low: int):
        """Generate machine-parseable JSON summary for workflow badge extraction"""

        # Map display names to tool_stats keys
        tool_key_map = {
            'Gitleaks': 'gitleaks',
            'TruffleHog': 'trufflehog',
            'Semgrep': 'semgrep',
            'Hadolint': 'hadolint',
            'ShellCheck': 'shellcheck',
            'yamllint': 'yamllint',
            'actionlint': 'actionlint',
            'kube-linter': 'kube-linter',
            'RBAC Analyzer': 'rbac'
        }

        # Calculate per-tool severity breakdowns
        tool_breakdowns = {}
        for tool_name in ['Gitleaks', 'TruffleHog', 'Semgrep', 'Hadolint', 'ShellCheck', 'yamllint', 'actionlint', 'kube-linter', 'RBAC Analyzer']:
            stats_key = tool_key_map[tool_name]
            tool_breakdowns[tool_name] = {
                'status': self.tool_stats.get(stats_key, {}).get('status', 'UNKNOWN'),
                'total': 0,
                'critical': 0,
                'high': 0,
                'medium': 0,
                'low': 0,
                'info': 0
            }

            # Count findings per severity for this tool
            for severity in ['critical', 'high', 'medium', 'low', 'info']:
                count = sum(1 for f in self.findings.get(severity, []) if f.get('tool') == tool_name)
                tool_breakdowns[tool_name][severity] = count
                tool_breakdowns[tool_name]['total'] += count

        # Add yamllint as code quality (separate from security findings)
        yamllint_summary = {
            'total': self.tool_stats.get('yamllint', {}).get('findings', 0),
            'errors': len([f for f in self.tool_stats.get('yamllint', {}).get('findings_data_all', []) if f.get('level') == 'error']),
            'warnings': len([f for f in self.tool_stats.get('yamllint', {}).get('findings_data_all', []) if f.get('level') == 'warning']),
            'status': self.tool_stats.get('yamllint', {}).get('status', 'SKIPPED')
        }

        # Calculate AI measurement metrics
        metrics = {
            'security_density': {
                'critical_per_scan': critical,
                'high_per_scan': high,
                'total_security_findings': total,
                'security_tools_run': sum(1 for t in tool_breakdowns.values() if t['status'] not in ['⏭️ SKIPPED', 'UNKNOWN'])
            },
            'code_quality_density': {
                'yamllint_total': yamllint_summary['total'],
                'yamllint_errors': yamllint_summary['errors'],
                'yamllint_warnings': yamllint_summary['warnings']
            },
            'remediation_priority': {
                'immediate_action_required': critical > 0,
                'high_priority_count': critical + high,
                'medium_priority_count': medium,
                'low_priority_count': low
            },
            'trend_indicators': {
                'has_critical_secrets': tool_breakdowns.get('Gitleaks', {}).get('critical', 0) > 0 or tool_breakdowns.get('TruffleHog', {}).get('critical', 0) > 0,
                'has_verified_secrets': tool_breakdowns.get('TruffleHog', {}).get('critical', 0) > 0,
                'has_rbac_issues': tool_breakdowns.get('RBAC Analyzer', {}).get('total', 0) > 0,
                'has_code_quality_issues': yamllint_summary['total'] > 0
            }
        }

        summary = {
            'format_version': '1.0',
            'generated': datetime.now(timezone.utc).strftime('%Y-%m-%d %H:%M:%S UTC'),
            'commit': self.github.get('sha', 'unknown'),
            'branch': self.github.get('ref_name', 'unknown'),
            'repository': self.github.get('repository', 'unknown'),
            'posture': posture,
            'total_findings': total,
            'severity_counts': {
                'critical': critical,
                'high': high,
                'medium': medium,
                'low': low,
                'info': len(self.findings.get('info', []))
            },
            'tools': tool_breakdowns,
            'code_quality': {
                'yamllint': yamllint_summary
            },
            'metrics': metrics
        }

        try:
            with open(output_file, 'w') as f:
                json.dump(summary, f, indent=2)
        except IOError as e:
            print(f"[ERROR] Failed to write JSON summary to {output_file}: {str(e)}", file=sys.stderr)
            sys.exit(1)

    def _generate_yamllint_report(self, output_file: str):
        """Generate dedicated yamllint report with all findings"""

        yamllint_stats = self.tool_stats.get('yamllint', {})
        if yamllint_stats.get('findings', 0) == 0:
            return  # Skip if no yamllint findings

        try:
            with open(output_file, 'w') as f:
                f.write(f"# YAMLlint Code Quality Report\n\n")
                f.write(f"**Generated:** {datetime.now(timezone.utc).strftime('%Y-%m-%d %H:%M:%S UTC')}\n\n")
                f.write(f"**Repository:** {self.github.get('repository', 'unknown')}\n\n")
                f.write(f"**Commit:** {self.github.get('sha', 'unknown')}\n\n")
                f.write(f"**Branch:** {self.github.get('ref_name', 'unknown')}\n\n")
                f.write(f"---\n\n")

                total = yamllint_stats.get('findings', 0)
                f.write(f"## Summary\n\n")
                f.write(f"**Total Issues:** {total}\n\n")

                # Use ALL findings for dedicated report (not truncated)
                yamllint_findings = yamllint_stats.get('findings_data_all', [])
                errors = [fi for fi in yamllint_findings if fi.get('level') == 'error']
                warnings = [fi for fi in yamllint_findings if fi.get('level') == 'warning']

                f.write(f"- Errors: {len(errors)}\n")
                f.write(f"- Warnings: {len(warnings)}\n\n")

                f.write(f"---\n\n")

                if errors:
                    f.write(f"## Errors ({len(errors)})\n\n")
                    for i, finding in enumerate(errors, 1):
                        f.write(f"### {i}. {finding['rule']}\n\n")
                        f.write(f"- **File:** `{finding['file']}:{finding['line']}`\n")
                        f.write(f"- **Description:** {finding['description']}\n\n")

                if warnings:
                    f.write(f"## Warnings ({len(warnings)})\n\n")
                    for i, finding in enumerate(warnings, 1):
                        f.write(f"### {i}. {finding['rule']}\n\n")
                        f.write(f"- **File:** `{finding['file']}:{finding['line']}`\n")
                        f.write(f"- **Description:** {finding['description']}\n\n")

                f.write(f"---\n\n")
                f.write(f"## Remediation\n\n")
                f.write(f"These are YAML style and formatting issues, not security vulnerabilities.\n\n")
                f.write(f"To fix automatically (where possible):\n")
                f.write(f"```bash\n")
                f.write(f"# Install yamllint\n")
                f.write(f"pip install yamllint\n\n")
                f.write(f"# Check current issues\n")
                f.write(f"yamllint .\n\n")
                f.write(f"# Many issues can be fixed manually or with automated formatters\n")
                f.write(f"```\n\n")
                f.write(f"Common fixes:\n")
                f.write(f"- **line-length**: Break long lines, use YAML multi-line strings\n")
                f.write(f"- **trailing-spaces**: Remove whitespace at end of lines\n")
                f.write(f"- **indentation**: Use consistent 2-space indentation\n")
                f.write(f"- **truthy**: Use `true`/`false` instead of `yes`/`no`\n\n")

        except IOError as e:
            print(f"[ERROR] Failed to write yamllint report to {output_file}: {str(e)}", file=sys.stderr)


def main():
    parser = argparse.ArgumentParser(description='Generate comprehensive security scan report')
    parser.add_argument('--output', default='security-report.md', help='Output file path')
    parser.add_argument('--json-summary', default=None, help='JSON summary output file for workflow parsing')
    parser.add_argument('--yamllint-report', default=None, help='Dedicated yamllint report output file (all findings)')
    parser.add_argument('--workspace', default='.', help='Workspace directory')
    parser.add_argument('--yamllint-limit', type=int, default=50, help='Maximum yamllint findings to show in comprehensive report (default: 50)')
    args = parser.parse_args()

    # Gather GitHub context from environment
    server_url = os.getenv('GITHUB_SERVER_URL', '')
    repository = os.getenv('GITHUB_REPOSITORY', '')
    run_id = os.getenv('GITHUB_RUN_ID', '')

    if server_url and repository and run_id:
        run_url = f"{server_url}/{repository}/actions/runs/{run_id}"
    else:
        run_url = 'N/A'

    github_context = {
        'repository': repository or 'unknown',
        'sha': os.getenv('GITHUB_SHA', 'unknown'),
        'ref_name': os.getenv('GITHUB_REF_NAME', 'unknown'),
        'run_url': run_url
    }

    try:
        generator = SecurityReportGenerator(args.workspace, github_context, yamllint_limit=args.yamllint_limit)
        generator.generate_report(args.output, args.json_summary, args.yamllint_report)
        print(f"✅ Comprehensive security report generated: {args.output}")
        if args.json_summary:
            print(f"✅ JSON summary generated: {args.json_summary}")
        if args.yamllint_report:
            print(f"✅ Dedicated yamllint report generated: {args.yamllint_report}")
    except Exception as e:
        print(f"❌ Failed to generate security report: {str(e)}", file=sys.stderr)
        sys.exit(1)


if __name__ == '__main__':
    main()
