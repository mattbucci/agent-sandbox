"""
Hindsight extensions that enforce the agent sandbox's per-agent memory policy.

Loaded into the Hindsight service by `sandbox-ctl memory start`:

    HINDSIGHT_API_TENANT_EXTENSION=sandbox_memory_policy:SandboxTenantExtension
    HINDSIGHT_API_TENANT_POLICY_FILE=<state>/hindsight/policy.json
    HINDSIGHT_API_OPERATION_VALIDATOR_EXTENSION=sandbox_memory_policy:SandboxPolicyValidator
    HINDSIGHT_API_OPERATION_VALIDATOR_POLICY_FILE=<state>/hindsight/policy.json

SandboxTenantExtension turns a bearer token into a principal (an agent type, or
"admin") and records it as the request's tenant_id — the identity Hindsight
carries into REST handlers, MCP tools and the background tasks a request starts.

SandboxPolicyValidator answers Hindsight's authorization hooks from the policy:
which banks a principal may use, the tag scope its reads are confined to (which
is also which mental models it can see), the tags it may write, and which MCP
tools it is offered. Anything that reshapes a bank — its config, its mental
models, deleting data — is admin-only.

The decisions themselves live in memory_policy.py; this file only adapts them
to Hindsight's interfaces.
"""

from hindsight_api.config import get_config
from hindsight_api.engine.search.tags import TagGroupLeaf
from hindsight_api.extensions.operation_validator import (
    BankListScope,
    OperationValidatorExtension,
    ValidationResult,
)
from hindsight_api.extensions.tenant import AuthenticationError, Tenant, TenantContext, TenantExtension

from memory_policy import ADMIN, PolicyFile

_POLICIES = {}


def _policy_file(config):
    path = (config or {}).get("policy_file")
    if not path:
        raise ValueError("policy_file is required (HINDSIGHT_API_<EXTENSION>_POLICY_FILE)")
    if path not in _POLICIES:
        _POLICIES[path] = PolicyFile(path)
    return _POLICIES[path]


class SandboxTenantExtension(TenantExtension):
    """Authenticate a per-agent (or the admin) bearer token."""

    def __init__(self, config):
        super().__init__(config)
        self._policy = _policy_file(config)

    async def authenticate(self, context):
        principal = self._policy.get().authenticate(context.api_key)
        if principal is None:
            raise AuthenticationError("Invalid API key")
        context.tenant_id = principal.name
        return TenantContext(schema_name=get_config().database_schema)

    async def list_tenants(self):
        return [Tenant(schema=get_config().database_schema)]


_DENIED = "not permitted by the sandbox memory policy"


class SandboxPolicyValidator(OperationValidatorExtension):
    """Authorize operations against the per-agent policy."""

    def __init__(self, config):
        super().__init__(config)
        self._policy = _policy_file(config)

    # ---- who is asking
    def _principal(self, request_context):
        """(unrestricted, principal). Background work Hindsight starts itself
        (consolidation, scheduled refreshes) is internal and unrestricted;
        everything else must resolve to a principal or is refused."""
        if request_context is None or request_context.internal:
            return True, None
        policy = self._policy.get()
        principal = policy.principal(request_context.tenant_id) or policy.authenticate(request_context.api_key)
        if principal is not None and principal.admin:
            return True, principal
        return False, principal

    def _access(self, request_context, bank_id):
        """(unrestricted, BankAccess-or-None)."""
        unrestricted, principal = self._principal(request_context)
        if unrestricted:
            return True, None
        return False, principal.bank(bank_id) if principal else None

    def _read_gate(self, request_context, bank_id):
        unrestricted, access = self._access(request_context, bank_id)
        if unrestricted or (access is not None and access.can_read):
            return ValidationResult.accept()
        return ValidationResult.reject(f"reading bank '{bank_id}' is {_DENIED}")

    def _admin_gate(self, request_context, what):
        unrestricted, _ = self._principal(request_context)
        if unrestricted:
            return ValidationResult.accept()
        return ValidationResult.reject(f"{what} is {_DENIED}")

    # ---- core operations
    async def precheck(self, ctx):
        op = str(getattr(ctx.operation, "value", ctx.operation))
        if op in ("mental_model_create", "mental_model_refresh"):
            return self._admin_gate(ctx.request_context, "managing mental models")
        if op in ("retain", "files_retain"):
            unrestricted, access = self._access(ctx.request_context, ctx.bank_id)
            if unrestricted or (access is not None and access.can_write):
                return ValidationResult.accept()
            return ValidationResult.reject(f"writing to bank '{ctx.bank_id}' is {_DENIED}")
        return self._read_gate(ctx.request_context, ctx.bank_id)

    async def validate_retain(self, ctx):
        unrestricted, access = self._access(ctx.request_context, ctx.bank_id)
        if unrestricted:
            return ValidationResult.accept()
        if access is None or not access.can_write:
            return ValidationResult.reject(f"writing to bank '{ctx.bank_id}' is {_DENIED}")
        # An agent's writes always carry a tag it owns: untagged means shared,
        # and only the admin writes there. Items that name tags keep them and
        # are checked against the write scope by Hindsight itself.
        contents, changed = [], False
        for item in ctx.contents:
            if not item.get("tags") and access.default_tags:
                item = {**item, "tags": list(access.default_tags)}
                changed = True
            elif not access.may_write_tags(item.get("tags")):
                return ValidationResult.reject(
                    f"writing tags {item.get('tags')} to bank '{ctx.bank_id}' is {_DENIED}")
            contents.append(item)
        return ValidationResult.accept_with(contents=contents) if changed else ValidationResult.accept()

    async def validate_recall(self, ctx):
        return self._read_gate(ctx.request_context, ctx.bank_id)

    async def validate_reflect(self, ctx):
        return self._read_gate(ctx.request_context, ctx.bank_id)

    async def validate_consolidate(self, ctx):
        return self._admin_gate(ctx.request_context, "running consolidation")

    # ---- tag scopes: what inside a bank a caller reaches
    async def resolve_tag_scope(self, ctx):
        unrestricted, access = self._access(ctx.request_context, ctx.bank_id)
        if unrestricted:
            return None
        if access is None:
            return [TagGroupLeaf(tags=["\x00none"], match="any_strict")]
        scope = access.read_scope()
        return None if scope is None else [TagGroupLeaf(tags=scope["tags"], match=scope["match"])]

    async def resolve_write_tag_scope(self, ctx):
        unrestricted, access = self._access(ctx.request_context, ctx.bank_id)
        if unrestricted:
            return None
        return list(access.write) if access is not None and access.write else ["\x00none"]

    # ---- mental models and everything else in a bank
    async def validate_mental_model_get(self, ctx):
        # Which models are visible is decided by the read tag scope.
        return self._read_gate(ctx.request_context, ctx.bank_id)

    async def validate_mental_model_refresh(self, ctx):
        return self._admin_gate(ctx.request_context, "managing mental models")

    async def validate_memory_update(self, ctx):
        return self._admin_gate(ctx.request_context, "editing memories")

    async def validate_bank_read(self, ctx):
        return self._read_gate(ctx.request_context, ctx.bank_id)

    async def validate_bank_write(self, ctx):
        return self._admin_gate(ctx.request_context, f"{getattr(ctx.operation, 'value', ctx.operation)}")

    async def validate_create_bank(self, ctx):
        return self._admin_gate(ctx.request_context, "creating banks")

    async def validate_import(self, ctx):
        return self._admin_gate(ctx.request_context, "importing")

    async def bank_list_scope(self, request_context):
        unrestricted, principal = self._principal(request_context)
        if unrestricted:
            return BankListScope(bank_ids=None)
        return BankListScope(bank_ids=sorted(principal.banks) if principal else [])

    async def filter_mcp_tools(self, bank_id, request_context, tools):
        unrestricted, principal = self._principal(request_context)
        if unrestricted:
            return tools
        access = principal.bank(bank_id) if principal else None
        if access is None or not access.can_read:
            return frozenset()
        allowed = tools if principal.mcp_tools is None else tools & principal.mcp_tools
        if not access.can_write:
            allowed = allowed - {"retain", "sync_retain"}
        return frozenset(allowed)
