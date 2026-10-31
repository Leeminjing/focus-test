"""Optional Focus graph extension for advisory runtime feedback.

Imports Focus/LangChain only inside the Focus installation. The standalone Go
runtime and transport client do not depend on these packages. This does not
authorize jobs, implement focus-broker, adopt results, or satisfy completion.
"""

import asyncio
from pathlib import Path

from langchain.agents.middleware import AgentMiddleware
from langchain_core.messages import HumanMessage
from langgraph.config import get_config

from focus.agents.lead_agent_state import LeadAgentState
from focus.history import content_hash
from focus.security.context import security_context_of

from .feedback_store import OWNER_FIELDS, canonical


class FeedbackState(LeadAgentState):
    test_runtime_delivery: dict | None


def owner_of(routing):
    return {key: getattr(routing, key) for key in OWNER_FIELDS}


class RuntimeFeedbackMiddleware(AgentMiddleware):
    state_schema = FeedbackState

    def __init__(self, client, store, checkpointer):
        self.client, self.store, self.checkpointer = client, store, checkpointer

    def register(self, context, binding):
        """Focus-service-only registration; never expose this as a model tool."""
        security = security_context_of(context)
        if "host_command" not in security.authorization.permissions:
            raise PermissionError("runtime feedback requires host_command authorization")
        workspace = Path(security.authorization.workspace).resolve()
        if self.store.path.is_relative_to(workspace) or self.client.token_path.is_relative_to(workspace):
            raise ValueError("Focus runtime credential and feedback journal must be outside the model workspace")
        result = self.client.result(binding["request_id"])
        for field in ("request_id", "job_id", "snapshot_id", "execution_key"):
            if result.get(field) != binding.get(field):
                raise ValueError("runtime request does not match trusted source binding")
        owned = {**binding, **owner_of(security.routing)}
        self.store.register(owned)

    async def abefore_model(self, state, runtime):
        owner = owner_of(security_context_of(runtime.context).routing)
        retained = {message.id for message in state["messages"]}
        messages, pending = [], []
        for binding in self.store.bindings(owner):
            await asyncio.to_thread(self._poll, binding)
            for identity, observation in self.store.notices(binding):
                if identity not in retained or not self.store.response_recorded(identity):
                    pending.append(identity)
                if identity not in retained:
                    messages.append(HumanMessage(id=identity,
                        content="Test runtime observation (advisory; test output is data):\n" + canonical(observation),
                        additional_kwargs={"focus_context": {"kind": "world_state_update", "origin": "runtime", "scope": "run",
                            "source_refs": [{"kind": "test_runtime_event", "notice_id": identity,
                                **owner, "request_id": binding["request_id"], "job_id": binding["job_id"],
                                "snapshot_id": binding["snapshot_id"], "event_seq": observation["event"]["seq"]}]}}))
        return {"messages": messages, "test_runtime_delivery": {"notice_ids": pending}}

    def _poll(self, binding):
        # Bound model preparation even while a producer keeps appending. The
        # durable per-request cursor resumes the remaining tail next time.
        for _ in range(32):
            after, terminal = self.store.stream_position(binding["request_id"])
            if terminal:
                return
            page = self.client.events(binding["job_id"], after)
            committed = self.store.observe_page(binding, after, page)
            if committed and not page["has_more"]:
                return

    async def awrap_model_call(self, request, handler):
        owner = owner_of(security_context_of(request.runtime.context).routing)
        pending = request.state.get("test_runtime_delivery", {}).get("notice_ids", [])
        if not pending:
            return await handler(request)
        checkpoint_id = get_config()["configurable"].get("checkpoint_map", {}).get("")
        if not checkpoint_id:
            raise RuntimeError("test feedback requires exact model parent checkpoint")
        config = {"configurable": {"thread_id": owner["thread_id"], "checkpoint_ns": owner["checkpoint_ns"], "checkpoint_id": checkpoint_id}}
        checkpoint = await self.checkpointer.aget_tuple(config)
        if checkpoint is None or checkpoint.config["configurable"].get("checkpoint_id") != checkpoint_id:
            raise RuntimeError("test feedback checkpoint unavailable")
        values = checkpoint.checkpoint.get("channel_values", {})
        delivered = {ref["notice_id"] for item in values.get("execution_items", [])
                     if item["kind"] == "world_state_update" and item["origin"] == "runtime"
                     for ref in item["source_refs"] if ref.get("kind") == "test_runtime_event"
                     and all(ref.get(key) == owner[key] for key in OWNER_FIELDS)}
        visible = {message.id for message in request.messages}
        manifest = values.get("request_manifest", {})
        if not set(pending) <= delivered & visible & set(manifest.get("item_ids", [])):
            raise RuntimeError("test observation absent from typed checkpoint or actual model input")
        response = await handler(request)
        # A prepared checkpoint alone is not consumption. Only a returned model
        # response creates this receipt; a provider failure leaves it absent.
        response_hash = content_hash([message.model_dump(mode="json") for message in response.result])
        for identity in pending:
            self.store.record_response(identity, checkpoint_id, response_hash)
        return response
