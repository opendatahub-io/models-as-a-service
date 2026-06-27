#!/usr/bin/env python3
# Plugin Version: 2.3.0
# DO NOT REMOVE: This comment is used for version detection during plugin updates
"""
Interactive Security Findings Acknowledgment Tool

Allows teams to interactively acknowledge security findings that are false positives
or accepted risks. Updates .github/config/security-baseline.yaml with detailed justifications.

Usage:
    python .github/scripts/acknowledge-findings.py
    python .github/scripts/acknowledge-findings.py --tool gitleaks
    python .github/scripts/acknowledge-findings.py --team security-team

Supports all 9 security tools:
    - Gitleaks (secrets)
    - TruffleHog (verified credentials)
    - Semgrep (SAST)
    - ShellCheck (shell scripts)
    - Hadolint (Dockerfiles)
    - yamllint (YAML validation)
    - actionlint (GitHub Actions)
    - kube-linter (Kubernetes manifests)
    - RBAC Analyzer (privilege chains)
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
from typing import Dict, List, Any, Optional, Tuple

class FindingAcknowledger:
    """Interactive tool for acknowledging security findings"""

    def __init__(self, workspace: str = '.', team: Optional[str] = None):
        self.workspace = Path(workspace)
        self.team = team or os.getenv('USER', 'unknown-user')
        self.baseline_data = {}
        self.available_tools = []
        self.baseline_path = self.workspace / '.github' / 'config' / 'security-baseline.yaml'

    def detect_available_findings(self) -> Dict[str, Path]:
        """Detect which tool output files exist in workspace

        Returns:
            Dict mapping tool name to file path
        """
        tool_files = {
            'gitleaks': 'gitleaks.json',
            'trufflehog': 'trufflehog.json',
            'semgrep': 'semgrep.sarif',
            'shellcheck': 'shellcheck.json',
            'hadolint': 'hadolint.sarif',
            'yamllint': 'yamllint.txt',
            'actionlint': 'actionlint.txt',
            'kube-linter': 'kube-linter.json',
            'rbac-analyzer': 'rbac-analysis.md'
        }

        found = {}
        for tool, filename in tool_files.items():
            filepath = self.workspace / filename
            if filepath.exists() and filepath.stat().st_size > 0:
                found[tool] = filepath
                self.available_tools.append(tool)

        return found

    def _create_empty_baseline(self) -> Dict:
        """Create empty baseline structure"""
        return {
            'version': '2.0',
            'description': 'Acknowledged security findings that are not real issues',
            '_comment': 'Findings acknowledged by teams using CLI tool or Claude skill',
            'gitleaks': [],
            'trufflehog': [],
            'semgrep': [],
            'shellcheck': [],
            'hadolint': [],
            'yamllint': [],
            'actionlint': [],
            'kube-linter': [],
            'rbac-analyzer': []
        }

    def _validate_baseline_schema(self, baseline_data: Dict) -> bool:
        """Validate baseline has required structure and all fields are valid

        Args:
            baseline_data: Baseline dictionary to validate

        Returns:
            True if baseline is valid, False otherwise
        """
        if not isinstance(baseline_data, dict):
            print(f"   ❌ Baseline must be a dictionary, got {type(baseline_data)}")
            return False

        # Required top-level keys
        required_keys = ['version']
        for key in required_keys:
            if key not in baseline_data:
                print(f"   ❌ Missing required key: '{key}'")
                return False

        # Check version format
        if not isinstance(baseline_data.get('version'), str):
            print(f"   ❌ 'version' must be a string")
            return False

        # All tool keys should be lists if present
        tool_keys = ['gitleaks', 'trufflehog', 'semgrep', 'shellcheck',
                     'hadolint', 'yamllint', 'actionlint', 'kube-linter', 'rbac-analyzer']

        for tool_key in tool_keys:
            if tool_key in baseline_data:
                if not isinstance(baseline_data[tool_key], list):
                    print(f"   ❌ '{tool_key}' must be a list, got {type(baseline_data[tool_key])}")
                    return False

                # Validate each finding in the list has required fields
                for i, finding in enumerate(baseline_data[tool_key]):
                    if not isinstance(finding, dict):
                        print(f"   ❌ {tool_key}[{i}] must be a dictionary, got {type(finding)}")
                        return False

                    # Most findings should have at minimum a hash or rule_id
                    if 'hash' not in finding and 'rule_id' not in finding and 'RuleId' not in finding:
                        print(f"   ⚠️  {tool_key}[{i}] missing identifier (hash/rule_id/RuleId)")
                        # Don't fail validation, just warn

        return True

    def load_baseline(self) -> None:
        """Load existing baseline from GitHub Secret (via environment) or create new

        Checks SECURITY_BASELINE environment variable (decompressed YAML from GitHub Actions)
        If not set, creates an empty baseline for initial setup

        No Vault dependency required - uses GitHub Secrets workflow
        """
        repo_name = self.workspace.name

        # Try to load from SECURITY_BASELINE environment variable
        # This is set by GitHub Actions after decompressing the GitHub Secret
        baseline_yaml = os.getenv('SECURITY_BASELINE')

        if baseline_yaml:
            # Baseline provided from GitHub Actions workflow
            try:
                self.baseline_data = yaml.safe_load(baseline_yaml)
                print(f"✅ Loaded baseline from GitHub Secret (via SECURITY_BASELINE env var)")

                # Validate baseline schema
                if not self._validate_baseline_schema(self.baseline_data):
                    print(f"⚠️  Baseline has invalid schema, creating new baseline")
                    self.baseline_data = self._create_empty_baseline()

                return
            except yaml.YAMLError as e:
                print(f"❌ Failed to parse baseline YAML from SECURITY_BASELINE environment variable")
                print(f"   Error: {e}")
                print(f"   The SECURITY_BASELINE secret may be corrupted or malformed")
                print(f"")
                print(f"   Troubleshooting steps:")
                print(f"   1. Verify the secret is properly compressed and encoded:")
                print(f"      gh secret get SECURITY_BASELINE --repo <repo> | base64 -d | gunzip | head")
                print(f"   2. Re-upload the baseline secret:")
                print(f"      cat baseline.yaml | gzip | base64 | gh secret set SECURITY_BASELINE --repo <repo> --body-file -")
                sys.exit(1)
            except Exception as e:
                print(f"❌ Unexpected error loading baseline from SECURITY_BASELINE")
                print(f"   Error: {e}")
                print(f"   Please report this issue with the error details above")
                sys.exit(1)

        # No baseline environment variable - check if baseline file exists locally
        baseline_file = self.workspace / '.github' / 'config' / 'security-baseline.yaml'
        if baseline_file.exists():
            # Load from file (user might be working on local baseline)
            try:
                with open(baseline_file) as f:
                    self.baseline_data = yaml.safe_load(f)
                print(f"✅ Loaded baseline from local file: {baseline_file}")
                print("   ⚠️  NOTE: This is a local copy. You'll need to compress and upload to GitHub Secret")

                # Validate baseline schema
                if not self._validate_baseline_schema(self.baseline_data):
                    print(f"⚠️  Baseline has invalid schema, creating new baseline")
                    self.baseline_data = self._create_empty_baseline()

                return
            except yaml.YAMLError as e:
                print(f"❌ Failed to parse local baseline YAML file: {baseline_file}")
                print(f"   Error: {e}")
                print(f"   The file may be corrupted or have invalid YAML syntax")
                print(f"   Creating new baseline instead")
                self.baseline_data = self._create_empty_baseline()
                return
            except FileNotFoundError:
                print(f"⚠️  Baseline file not found: {baseline_file}")
            except PermissionError:
                print(f"❌ Permission denied reading baseline file: {baseline_file}")
                print(f"   Check file permissions and try again")
                sys.exit(1)
            except Exception as e:
                print(f"❌ Unexpected error loading local baseline: {e}")
                print(f"   Creating new baseline instead")
                self.baseline_data = self._create_empty_baseline()
                return

        # No baseline found - create new empty baseline
        print(f"ℹ️  No baseline found for {repo_name}")
        print(f"   Creating new baseline")
        self.baseline_data = self._create_empty_baseline()

    def parse_gitleaks(self, filepath: Path) -> List[Dict[str, Any]]:
        """Parse Gitleaks JSON output"""
        findings = []
        try:
            with open(filepath) as f:
                data = json.load(f)
                if not data:
                    return []

                for item in data:
                    # Normalize file path
                    file_path = item.get('File', 'unknown')
                    if file_path.startswith('/repo/'):
                        file_path = file_path[6:]
                    file_path = os.path.normpath(file_path).lstrip('/')

                    description = item.get('Description', 'Secret detected')
                    desc_hash = hashlib.sha256(description.encode()).hexdigest()[:8]

                    findings.append({
                        'file': file_path,
                        'line': item.get('StartLine', '?'),
                        'rule': item.get('RuleID', 'unknown'),
                        'description_hash': desc_hash,
                        'description': description
                    })
        except Exception as e:
            print(f"Error parsing Gitleaks output: {e}", file=sys.stderr)

        return findings

    def parse_kube_linter(self, filepath: Path) -> List[Dict[str, Any]]:
        """Parse kube-linter JSON output

        Note: kube-linter v0.7.6+ structure has K8sObjectInfo fields under Object.K8sObject
        """
        findings = []
        try:
            with open(filepath) as f:
                data = json.load(f)
                reports = data.get('Reports', [])

                for report in reports:
                    # kube-linter v0.7.6+ structure: Object.K8sObject contains the resource info
                    obj = report.get('Object', {}).get('K8sObject', {})
                    findings.append({
                        'check': report.get('Check', 'unknown'),
                        'object': {
                            'kind': obj.get('GroupVersionKind', {}).get('Kind', 'unknown'),
                            'name': obj.get('Name', 'unknown'),
                            'namespace': obj.get('Namespace') or None
                        },
                        'message': report.get('Diagnostic', {}).get('Message', '')
                    })
        except Exception as e:
            print(f"Error parsing kube-linter output: {e}", file=sys.stderr)

        return findings

    def parse_shellcheck(self, filepath: Path) -> List[Dict[str, Any]]:
        """Parse ShellCheck JSON output"""
        findings = []
        try:
            with open(filepath) as f:
                data = json.load(f)

                for item in data:
                    findings.append({
                        'file': item.get('file', ''),
                        'line': item.get('line', 0),
                        'code': item.get('code', 0),
                        'message': item.get('message', ''),
                        'level': item.get('level', 'warning')
                    })
        except Exception as e:
            print(f"Error parsing ShellCheck output: {e}", file=sys.stderr)

        return findings

    def parse_trufflehog(self, filepath: Path) -> List[Dict[str, Any]]:
        """Parse TruffleHog JSONL output"""
        findings = []
        try:
            with open(filepath) as f:
                for line in f:
                    if not line.strip():
                        continue

                    item = json.loads(line)

                    # Extract file and line from nested structure
                    fs_data = item.get('SourceMetadata', {}).get('Data', {}).get('Filesystem', {})
                    file_path = fs_data.get('file', '')
                    line_num = fs_data.get('line', 0)

                    raw_secret = item.get('Raw', '')

                    findings.append({
                        'detector': item.get('DetectorName', ''),
                        'file': file_path,
                        'line': line_num,
                        'verified': item.get('Verified', False),
                        'raw': f"[REDACTED - {len(raw_secret)} chars]"  # Security: Never expose secret content
                    })
        except Exception as e:
            print(f"Error parsing TruffleHog output: {e}", file=sys.stderr)

        return findings

    def parse_yamllint(self, filepath: Path) -> List[Dict[str, Any]]:
        """Parse yamllint text output"""
        findings = []
        try:
            # Regex pattern: file:line:col: [level] message (rule)
            pattern = r'^(.+?):(\d+):(\d+): \[(\w+)\] (.+?) \((.+?)\)$'

            with open(filepath) as f:
                for line in f:
                    match = re.match(pattern, line.strip())
                    if match:
                        findings.append({
                            'file': match.group(1),
                            'line': int(match.group(2)),
                            'column': int(match.group(3)),
                            'level': match.group(4),
                            'message': match.group(5),
                            'rule': match.group(6)
                        })
        except Exception as e:
            print(f"Error parsing yamllint output: {e}", file=sys.stderr)

        return findings

    def parse_actionlint(self, filepath: Path) -> List[Dict[str, Any]]:
        """Parse actionlint text output"""
        findings = []
        try:
            # Regex patterns:
            # Pattern 1: filepath:line:col: message [rule]
            # Pattern 2: filepath:line:col: message (no rule)
            pattern_with_rule = r'^(.+?):(\d+):(\d+): (.+?) \[(.+?)\]$'
            pattern_no_rule = r'^(.+?):(\d+):(\d+): (.+?)$'

            with open(filepath) as f:
                for line in f:
                    line = line.strip()
                    # Try pattern with rule first
                    match = re.match(pattern_with_rule, line)
                    if match:
                        findings.append({
                            'file': match.group(1),
                            'line': int(match.group(2)),
                            'column': int(match.group(3)),
                            'message': match.group(4),
                            'rule': match.group(5)
                        })
                    else:
                        # Try pattern without rule
                        match = re.match(pattern_no_rule, line)
                        if match:
                            findings.append({
                                'file': match.group(1),
                                'line': int(match.group(2)),
                                'column': int(match.group(3)),
                                'message': match.group(4),
                                'rule': None
                            })
        except Exception as e:
            print(f"Error parsing actionlint output: {e}", file=sys.stderr)

        return findings

    def _parse_sarif(self, filepath: Path, tool_name: str) -> List[Dict[str, Any]]:
        """Helper method to parse SARIF format output (used by Semgrep and Hadolint)"""
        findings = []
        try:
            with open(filepath) as f:
                data = json.load(f)

            for run in data.get('runs', []):
                for result in run.get('results', []):
                    # Extract location (may have multiple, use first)
                    locations = result.get('locations', [])
                    if not locations:
                        continue

                    phys_loc = locations[0].get('physicalLocation', {})
                    artifact = phys_loc.get('artifactLocation', {})
                    region = phys_loc.get('region', {})

                    findings.append({
                        'rule_id': result.get('ruleId', ''),
                        'file': artifact.get('uri', ''),
                        'line': region.get('startLine', 0),
                        'message': result.get('message', {}).get('text', ''),
                        'level': result.get('level', 'warning')
                    })
        except Exception as e:
            print(f"Error parsing {tool_name} SARIF output: {e}", file=sys.stderr)

        return findings

    def parse_semgrep(self, filepath: Path) -> List[Dict[str, Any]]:
        """Parse Semgrep SARIF output"""
        return self._parse_sarif(filepath, 'Semgrep')

    def parse_hadolint(self, filepath: Path) -> List[Dict[str, Any]]:
        """Parse Hadolint SARIF output"""
        return self._parse_sarif(filepath, 'Hadolint')

    def parse_rbac_analyzer(self, filepath: Path) -> List[Dict[str, Any]]:
        """Parse RBAC Analyzer Markdown output"""
        findings = []
        try:
            with open(filepath) as f:
                content = f.read()

            # Parse markdown sections by severity
            severity_pattern = r'### (CRITICAL|HIGH|WARNING|INFO) \((\d+) findings?\)'
            finding_pattern = r'\d+\. \*\*(.+?)\*\*\s*\n\s*- File: `(.+?)`\s*\n\s*- Issue: (.+?)\n\s*- Fix: (.+?)(?=\n\n|\n\d+\.|\Z)'

            # Split by severity sections
            sections = re.split(severity_pattern, content)

            for i in range(1, len(sections), 3):
                severity = sections[i]
                section_content = sections[i+2] if i+2 < len(sections) else ''

                # Extract findings from this severity section
                for match in re.finditer(finding_pattern, section_content, re.DOTALL):
                    findings.append({
                        'severity': severity,
                        'title': match.group(1).strip(),
                        'file': match.group(2).strip(),
                        'issue': match.group(3).strip(),
                        'fix': match.group(4).strip()
                    })
        except Exception as e:
            print(f"Error parsing RBAC Analyzer output: {e}", file=sys.stderr)

        return findings

    def filter_new_findings(self, tool: str, all_findings: List[Dict]) -> List[Dict]:
        """Filter out findings that are already in baseline

        Returns:
            List of findings not in baseline
        """
        baseline_findings = self.baseline_data.get(tool, [])
        new_findings = []

        for finding in all_findings:
            is_acknowledged = False

            # Check if this finding matches any baseline entry
            for baseline_entry in baseline_findings:
                if self._findings_match(tool, finding, baseline_entry):
                    is_acknowledged = True
                    break

            if not is_acknowledged:
                new_findings.append(finding)

        return new_findings

    def _findings_match(self, tool: str, finding: Dict, baseline_entry: Dict) -> bool:
        """Check if a finding matches a baseline entry

        Each tool has different matching criteria based on unique identifiers
        """
        # Helper: normalize line numbers for comparison (handles int vs str mismatch)
        def normalize_line(value):
            """Convert line number to string for comparison, handle None"""
            return None if value is None else str(value)

        if tool == 'gitleaks':
            return (finding.get('file') == baseline_entry.get('file') and
                    normalize_line(finding.get('line')) == normalize_line(baseline_entry.get('line')) and
                    finding.get('rule') == baseline_entry.get('rule') and
                    finding.get('description_hash') == baseline_entry.get('description_hash'))

        elif tool == 'kube-linter':
            obj1 = finding.get('object', {})
            obj2 = baseline_entry.get('object', {})
            return (finding.get('check') == baseline_entry.get('check') and
                    obj1.get('kind') == obj2.get('kind') and
                    obj1.get('name') == obj2.get('name') and
                    obj1.get('namespace') == obj2.get('namespace'))

        elif tool == 'shellcheck':
            return (finding.get('file') == baseline_entry.get('file') and
                    normalize_line(finding.get('line')) == normalize_line(baseline_entry.get('line')) and
                    finding.get('code') == baseline_entry.get('code'))

        elif tool == 'trufflehog':
            return (finding.get('detector') == baseline_entry.get('detector') and
                    finding.get('file') == baseline_entry.get('file') and
                    normalize_line(finding.get('line')) == normalize_line(baseline_entry.get('line')))

        elif tool == 'yamllint':
            return (finding.get('file') == baseline_entry.get('file') and
                    normalize_line(finding.get('line')) == normalize_line(baseline_entry.get('line')) and
                    finding.get('rule') == baseline_entry.get('rule'))

        elif tool == 'actionlint':
            return (finding.get('file') == baseline_entry.get('file') and
                    normalize_line(finding.get('line')) == normalize_line(baseline_entry.get('line')) and
                    finding.get('message') == baseline_entry.get('message'))

        elif tool in ('semgrep', 'hadolint'):
            return (finding.get('rule_id') == baseline_entry.get('rule_id') and
                    finding.get('file') == baseline_entry.get('file') and
                    normalize_line(finding.get('line')) == normalize_line(baseline_entry.get('line')))

        elif tool == 'rbac-analyzer':
            return (finding.get('title') == baseline_entry.get('title') and
                    finding.get('file') == baseline_entry.get('file'))

        # Add more tool-specific matching logic as needed
        return False

    def interactive_acknowledge(self, tool: str, findings: List[Dict]) -> List[Dict]:
        """Interactive workflow to acknowledge findings

        Returns:
            List of acknowledged findings with reason and metadata
        """
        print(f"\n{'='*80}")
        print(f"📋 {tool.upper()} - New Findings")
        print(f"{'='*80}\n")

        if not findings:
            print("✅ No new findings to acknowledge\n")
            return []

        print(f"Found {len(findings)} new {tool} findings:\n")

        # Display findings
        for idx, finding in enumerate(findings, 1):
            print(f"[{idx}] ", end="")
            if tool == 'gitleaks':
                print(f"CRITICAL: {finding['rule']}")
                print(f"    File: {finding['file']}:{finding['line']}")
                print(f"    Description: {finding['description']}")
            elif tool == 'kube-linter':
                obj = finding['object']
                obj_id = f"{obj['kind']}/{obj['name']}"
                if obj['namespace']:
                    obj_id = f"{obj['kind']}/{obj['namespace']}/{obj['name']}"
                print(f"{finding['check']}")
                print(f"    Object: {obj_id}")
                print(f"    Message: {finding['message']}")
            elif tool == 'shellcheck':
                print(f"SC{finding['code']}: {finding['level'].upper()}")
                print(f"    File: {finding['file']}:{finding['line']}")
                print(f"    Message: {finding['message']}")
            elif tool == 'trufflehog':
                verified_status = "⛔ YES (ACTIVE CREDENTIAL)" if finding['verified'] else "✅ No"
                print(f"{finding['detector']}")
                print(f"    File: {finding['file']}:{finding['line']}")
                print(f"    Verified: {verified_status}")
                if not finding['verified']:
                    print(f"    Raw: {finding['raw']}")
            elif tool == 'yamllint':
                print(f"{finding['rule']}: {finding['level'].upper()}")
                print(f"    File: {finding['file']}:{finding['line']}:{finding['column']}")
                print(f"    Message: {finding['message']}")
            elif tool == 'actionlint':
                rule_info = f" [{finding['rule']}]" if finding.get('rule') else ""
                print(f"GitHub Actions Issue{rule_info}")
                print(f"    File: {finding['file']}:{finding['line']}:{finding['column']}")
                print(f"    Message: {finding['message']}")
            elif tool == 'semgrep':
                print(f"{finding['rule_id']}: {finding['level'].upper()}")
                print(f"    File: {finding['file']}:{finding['line']}")
                print(f"    Message: {finding['message']}")
            elif tool == 'hadolint':
                print(f"{finding['rule_id']}: {finding['level'].upper()}")
                print(f"    File: {finding['file']}:{finding['line']}")
                print(f"    Message: {finding['message']}")
            elif tool == 'rbac-analyzer':
                print(f"{finding['severity']}: {finding['title']}")
                print(f"    File: {finding['file']}")
                print(f"    Issue: {finding['issue']}")
                print(f"    Fix: {finding['fix']}")
            print()

        # Select findings to acknowledge
        while True:
            selection = input(f"Select findings to acknowledge (comma-separated, e.g., 1,2,3) or 'skip': ").strip()
            if selection.lower() == 'skip':
                return []

            try:
                indices = [int(x.strip()) for x in selection.split(',')]
                if all(1 <= idx <= len(findings) for idx in indices):
                    break
                else:
                    print(f"⚠️  Please enter numbers between 1 and {len(findings)}")
            except ValueError:
                print("⚠️  Invalid input. Please enter comma-separated numbers or 'skip'")

        # Acknowledge selected findings
        acknowledged = []
        for idx in indices:
            finding = findings[idx - 1]
            print(f"\n{'─'*80}")
            print(f"Acknowledging finding #{idx}")
            print(f"{'─'*80}")

            # Block verified TruffleHog secrets from being acknowledged
            if tool == 'trufflehog' and finding.get('verified'):
                print("  ⛔ VERIFIED SECRET - CANNOT ACKNOWLEDGE")
                print("  ⚠️  This is an active, usable credential")
                print("  📋 Action required: ROTATE this credential immediately")
                print("  ✅ Skipping to next finding...")
                continue

            # Collect reason
            while True:
                reason = input("\nReason (required, explain why this isn't a real issue): ").strip()
                if len(reason) >= 10:
                    break
                print("⚠️  Reason must be at least 10 characters. Explain why this is safe to ignore.")

            # Collect acknowledged_by
            acknowledged_by = input(f"Acknowledged by [{self.team}]: ").strip() or self.team

            # Add metadata
            finding['reason'] = reason
            finding['acknowledged_by'] = acknowledged_by
            finding['acknowledged_date'] = datetime.now(timezone.utc).strftime('%Y-%m-%d')

            # Remove temporary fields
            if 'description' in finding and tool == 'gitleaks':
                del finding['description']
            if 'message' in finding and tool == 'kube-linter':
                del finding['message']
            if 'raw' in finding and tool == 'trufflehog':
                del finding['raw']
            if 'issue' in finding and tool == 'rbac-analyzer':
                del finding['issue']
            if 'fix' in finding and tool == 'rbac-analyzer':
                del finding['fix']

            acknowledged.append(finding)
            print("✅ Finding acknowledged")

        return acknowledged

    def update_baseline(self, tool: str, new_acknowledgments: List[Dict]) -> None:
        """Update baseline file with new acknowledgments"""
        if tool not in self.baseline_data:
            self.baseline_data[tool] = []

        self.baseline_data[tool].extend(new_acknowledgments)

    def save_baseline(self) -> None:
        """Save baseline to GitHub Secret

        Outputs the baseline YAML and provides instructions for compressing
        and uploading to GitHub Secrets
        """
        import subprocess
        import base64
        import gzip

        repo_name = self.workspace.name

        # Convert baseline to YAML
        baseline_yaml = yaml.dump(
            self.baseline_data,
            default_flow_style=False,
            sort_keys=False,
            allow_unicode=True
        )

        # Save to temporary file
        self.baseline_path.parent.mkdir(parents=True, exist_ok=True)

        with open(self.baseline_path, 'w') as f:
            f.write(baseline_yaml)

        print(f"\n✅ Baseline saved to: {self.baseline_path}")

        # Calculate sizes
        uncompressed_size = len(baseline_yaml.encode())
        compressed_data = gzip.compress(baseline_yaml.encode())
        compressed_size = len(compressed_data)
        encoded_size = len(base64.b64encode(compressed_data))

        print(f"\n📊 Baseline Statistics:")
        print(f"   Uncompressed: {uncompressed_size} bytes")
        print(f"   Compressed: {compressed_size} bytes ({100 - (compressed_size * 100 // uncompressed_size)}% reduction)")
        print(f"   Base64 encoded: {encoded_size} bytes")
        print(f"   GitHub Secret Limit: 49,152 bytes")
        print(f"   Usage: {encoded_size * 100 // 49152}%")

        if encoded_size > 45000:
            print(f"\n⚠️  WARNING: Approaching GitHub secret size limit!")
            print(f"   Consider cleaning up old acknowledgments")

        # Detect if we're in a git repository
        is_git_repo = False
        repo_full_name = "owner/repo"

        try:
            # Check if we're in a git repository
            subprocess.run(
                ['git', 'rev-parse', '--git-dir'],
                cwd=self.workspace,
                capture_output=True,
                check=True,
                timeout=5
            )
            is_git_repo = True

            # Get repository info (remote URL)
            try:
                repo_info = subprocess.check_output(
                    ['git', 'config', '--get', 'remote.origin.url'],
                    cwd=self.workspace,
                    text=True,
                    timeout=5
                ).strip()

                # Extract owner/repo from git URL (supports both HTTPS and SSH formats)
                if 'github.com' in repo_info:
                    # Remove .git suffix
                    repo_info = repo_info.replace('.git', '')

                    # Handle SSH format: git@github.com:owner/repo
                    if 'git@github.com:' in repo_info:
                        # Extract after the colon
                        repo_path = repo_info.split('git@github.com:')[-1]
                        parts = repo_path.split('/')
                        if len(parts) >= 2:
                            repo_full_name = f"{parts[-2]}/{parts[-1]}"
                    # Handle HTTPS format: https://github.com/owner/repo
                    elif 'github.com/' in repo_info:
                        parts = repo_info.split('/')
                        if len(parts) >= 2:
                            repo_full_name = f"{parts[-2]}/{parts[-1]}"
            except subprocess.SubprocessError:
                # No remote configured, use placeholder
                pass
        except (subprocess.SubprocessError, FileNotFoundError):
            # Not in a git repository or git not installed
            is_git_repo = False

        # Show different instructions based on git repo detection
        if not is_git_repo:
            print(f"\n⚠️  WARNING: Not in a git repository")
            print(f"   You'll need to manually upload the baseline to GitHub Secrets")

        print(f"\n📝 Next Steps:")
        print(f"   1. Compress and encode the baseline:")
        print(f"")
        print(f"      cat {self.baseline_path} | gzip | base64 -w 0 > /tmp/baseline-compressed.txt")
        print(f"")
        print(f"   2. Upload to GitHub Secret:")
        print(f"")
        if is_git_repo:
            print(f"      gh secret set SECURITY_BASELINE -b \"$(cat /tmp/baseline-compressed.txt)\" --repo {repo_full_name}")
        else:
            print(f"      gh secret set SECURITY_BASELINE -b \"$(cat /tmp/baseline-compressed.txt)\" --repo YOUR_ORG/YOUR_REPO")
        print(f"")
        print(f"   3. Verify the secret was set:")
        print(f"")
        if is_git_repo:
            print(f"      gh secret list --repo {repo_full_name}")
        else:
            print(f"      gh secret list --repo YOUR_ORG/YOUR_REPO")
        print(f"")
        print(f"   4. Re-run security scan to verify acknowledgments:")
        print(f"")
        print(f"      gh workflow run security-full-scan.yml")
        print(f"")
        if is_git_repo:
            print(f"   5. Review changes (optional):")
            print(f"")
            print(f"      git diff {self.baseline_path}")
            print(f"")
            print(f"   6. Clean up temporary file:")
            print(f"")
            print(f"      rm {self.baseline_path}")
        else:
            print(f"   5. Clean up temporary file:")
            print(f"")
            print(f"      rm {self.baseline_path}")
        print(f"")
        print(f"   ⚠️  NOTE: Do NOT commit {self.baseline_path} to git!")
        print(f"   The baseline should only exist in GitHub Secrets")
        print(f"")

    def run_interactive(self, tool_filter: Optional[str] = None) -> None:
        """Run interactive acknowledgment workflow"""
        print(f"\n{'='*80}")
        print("🔒 Security Findings Acknowledgment Tool")
        print(f"{'='*80}\n")

        # Detect available findings
        available = self.detect_available_findings()
        if not available:
            print("❌ No security tool output files found in current directory.")
            print("\n📥 To use this tool:")
            print("1. Download workflow artifacts from failed security scan")
            print("2. Extract output files (gitleaks.json, kube-linter.json, etc.)")
            print("3. Run this tool from the directory containing the files\n")
            sys.exit(1)

        print(f"✅ Found output files for {len(available)} tools:")
        for tool in available:
            print(f"   - {tool}")
        print()

        # Load baseline
        self.load_baseline()
        if self.baseline_path.exists():
            total_acknowledged = sum(len(entries) for entries in self.baseline_data.values() if isinstance(entries, list))
            print(f"📋 Loaded existing baseline with {total_acknowledged} acknowledged findings\n")
        else:
            print("📋 No existing baseline - will create new file\n")

        # Filter tools if specified
        if tool_filter:
            if tool_filter not in available:
                print(f"❌ Tool '{tool_filter}' output not found")
                sys.exit(1)
            tools_to_process = [tool_filter]
        else:
            tools_to_process = self.available_tools

        # Process each tool
        total_acknowledged = 0
        acknowledgments_by_tool = {}

        for tool in tools_to_process:
            # Parse findings
            if tool == 'gitleaks':
                all_findings = self.parse_gitleaks(available[tool])
            elif tool == 'kube-linter':
                all_findings = self.parse_kube_linter(available[tool])
            elif tool == 'shellcheck':
                all_findings = self.parse_shellcheck(available[tool])
            elif tool == 'trufflehog':
                all_findings = self.parse_trufflehog(available[tool])
            elif tool == 'yamllint':
                all_findings = self.parse_yamllint(available[tool])
            elif tool == 'actionlint':
                all_findings = self.parse_actionlint(available[tool])
            elif tool == 'semgrep':
                all_findings = self.parse_semgrep(available[tool])
            elif tool == 'hadolint':
                all_findings = self.parse_hadolint(available[tool])
            elif tool == 'rbac-analyzer':
                all_findings = self.parse_rbac_analyzer(available[tool])
            else:
                # All parsers implemented
                print(f"⏭️  Skipping {tool} (parser not yet implemented)")
                continue

            # Filter new findings
            new_findings = self.filter_new_findings(tool, all_findings)

            # Interactive acknowledgment
            acknowledged = self.interactive_acknowledge(tool, new_findings)
            if acknowledged:
                self.update_baseline(tool, acknowledged)
                total_acknowledged += len(acknowledged)
                acknowledgments_by_tool[tool] = len(acknowledged)

        # Save baseline
        if total_acknowledged > 0:
            self.save_baseline()
            print(f"\n{'='*80}")
            print(f"✅ Acknowledged {total_acknowledged} findings:")
            for tool, count in acknowledgments_by_tool.items():
                print(f"   - {tool}: {count} finding(s)")
            print(f"\n📝 Updated {self.baseline_path}")
            print(f"\n{'='*80}")
            print("\n📋 Next steps:")
            print(f"1. Review changes: git diff {self.baseline_path}")
            print(f"2. Commit: git add {self.baseline_path} && git commit -m \"chore: Acknowledge security findings\"")
            print("3. Push to re-run security checks")
            print()
        else:
            print("\n✅ No findings acknowledged\n")


def main():
    parser = argparse.ArgumentParser(
        description='Interactively acknowledge security findings as false positives or accepted risks',
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
Examples:
  # Interactive mode (recommended)
  python .github/scripts/acknowledge-findings.py

  # Acknowledge only Gitleaks findings
  python .github/scripts/acknowledge-findings.py --tool gitleaks

  # Specify team name
  python .github/scripts/acknowledge-findings.py --team security-team

Workflow:
  1. Download security scan artifacts from failed workflow
  2. Extract output files to current directory
  3. Run this tool to interactively acknowledge false positives
  4. Commit updated .github/config/security-baseline.yaml
  5. Push to re-run security checks
        """
    )
    parser.add_argument('--tool', help='Process only specific tool (gitleaks, kube-linter, etc.)')
    parser.add_argument('--team', help='Team or person acknowledging findings (default: $USER)')
    parser.add_argument('--workspace', default='.', help='Workspace directory (default: current directory)')

    args = parser.parse_args()

    try:
        acknowledger = FindingAcknowledger(workspace=args.workspace, team=args.team)
        acknowledger.run_interactive(tool_filter=args.tool)
    except KeyboardInterrupt:
        print("\n\n⚠️  Interrupted by user - no changes saved\n")
        sys.exit(130)
    except Exception as e:
        print(f"\n❌ Error: {str(e)}", file=sys.stderr)
        sys.exit(1)


if __name__ == '__main__':
    main()
