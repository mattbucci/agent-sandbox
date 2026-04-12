#!/usr/bin/env python3
"""
Agent Sandbox Runtime — DeepAgents + LangGraph

Minimal agent that connects to a LiteLLM proxy (OpenAI-compatible API)
and uses LocalShellBackend for real bash execution inside the Firecracker VM.

Configuration is read from environment variables (set via /etc/agent.conf):
  LLM_API_BASE  - LiteLLM proxy URL (e.g., http://192.168.1.100:4000/v1)
  LLM_API_KEY   - API key for LiteLLM
  LLM_MODEL     - Model name (e.g., deepseek-coder-v2)
  AGENT_TYPE    - Agent specialization (debugger, feature-dev, etc.)
  AGENT_NAME    - Human-readable agent name
"""

import os
import sys
import asyncio
import logging

logging.basicConfig(
    level=logging.INFO,
    format="[%(asctime)s] %(name)s %(levelname)s: %(message)s",
    handlers=[
        logging.FileHandler("/var/log/agent.log"),
        logging.StreamHandler(sys.stdout),
    ],
)
logger = logging.getLogger("agent")


def load_system_prompt() -> str:
    """Load the agent's system prompt from /etc/agent/system-prompt.md"""
    prompt_path = "/etc/agent/system-prompt.md"
    if os.path.exists(prompt_path):
        with open(prompt_path) as f:
            return f.read().strip()
    return f"You are an AI agent of type: {os.environ.get('AGENT_TYPE', 'generic')}."


def create_agent():
    """Create and configure the DeepAgents agent."""
    from deepagents import create_deep_agent
    from deepagents.backends import LocalShellBackend

    api_base = os.environ.get("LLM_API_BASE", "http://localhost:4000/v1")
    api_key = os.environ.get("LLM_API_KEY", "sk-default")
    model_name = os.environ.get("LLM_MODEL", "default-model")
    agent_type = os.environ.get("AGENT_TYPE", "generic")

    logger.info(f"Creating {agent_type} agent")
    logger.info(f"LLM endpoint: {api_base}")
    logger.info(f"Model: {model_name}")

    # Set OpenAI-compatible env vars for LangChain
    os.environ["OPENAI_API_KEY"] = api_key
    os.environ["OPENAI_API_BASE"] = api_base

    system_prompt = load_system_prompt()
    logger.info(f"System prompt loaded ({len(system_prompt)} chars)")

    # Create the agent with LocalShellBackend
    # The Firecracker VM IS the sandbox, so unrestricted shell is safe
    agent = create_deep_agent(
        model=f"openai:{model_name}",
        backend=LocalShellBackend(
            cwd="/home/agent/workspace",
        ),
        system_prompt=system_prompt,
    )

    return agent


async def run_agent_loop(agent):
    """Run the agent in a loop, waiting for tasks via stdin or a task file."""
    agent_type = os.environ.get("AGENT_TYPE", "generic")
    agent_name = os.environ.get("AGENT_NAME", "Agent")

    logger.info(f"{agent_name} is ready and waiting for input...")

    # Check if there's an initial task file
    task_file = "/etc/agent/initial-task.md"
    if os.path.exists(task_file):
        with open(task_file) as f:
            initial_task = f.read().strip()
        if initial_task:
            logger.info(f"Found initial task ({len(initial_task)} chars)")
            result = await agent.ainvoke(
                {"messages": [{"role": "user", "content": initial_task}]}
            )
            logger.info("Initial task completed")
            # Log the result
            if "messages" in result:
                for msg in result["messages"]:
                    if hasattr(msg, "content"):
                        logger.info(f"Agent response: {msg.content[:500]}")

    # Interactive loop: read tasks from stdin or a watch directory
    task_dir = "/home/agent/tasks"
    os.makedirs(task_dir, exist_ok=True)

    logger.info(f"Watching {task_dir} for new tasks...")
    processed = set()

    while True:
        # Check for new task files
        try:
            for fname in sorted(os.listdir(task_dir)):
                fpath = os.path.join(task_dir, fname)
                if fpath in processed or not fname.endswith(".md"):
                    continue

                processed.add(fpath)
                logger.info(f"Processing task: {fname}")

                with open(fpath) as f:
                    task_content = f.read().strip()

                if not task_content:
                    continue

                result = await agent.ainvoke(
                    {"messages": [{"role": "user", "content": task_content}]}
                )

                # Write result
                result_path = fpath.replace(".md", ".result.md")
                if "messages" in result:
                    last_msg = result["messages"][-1]
                    if hasattr(last_msg, "content"):
                        with open(result_path, "w") as f:
                            f.write(last_msg.content)
                        logger.info(f"Result written to {result_path}")

        except Exception as e:
            logger.error(f"Error processing tasks: {e}")

        await asyncio.sleep(5)


def main():
    logger.info("Agent sandbox runtime starting...")
    agent = create_agent()
    asyncio.run(run_agent_loop(agent))


if __name__ == "__main__":
    main()
