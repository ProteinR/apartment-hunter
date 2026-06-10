import os
import sys
import argparse
import asyncio
from telethon import TelegramClient
from telethon.sessions import StringSession

# Rate limiting safeguard delay
DELAY_SECONDS = 4.0

def load_env(env_path):
    env = {}
    if not os.path.exists(env_path):
        return env
    with open(env_path, "r") as f:
        for line in f:
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            if "=" in line:
                k, v = line.split("=", 1)
                env[k.strip()] = v.strip()
    return env

async def send_message_with_delay(client, bot_entity, text):
    print(f"\n[User]: {text}")
    msg = await client.send_message(bot_entity, text)
    await asyncio.sleep(DELAY_SECONDS)
    
    # Fetch responses that came after our message
    messages = await client.get_messages(bot_entity, limit=5)
    new_msgs = [m for m in messages if m.id > msg.id and not m.out]
    for resp in reversed(new_msgs):
        print(f"[Bot]: {resp.text}")
    return msg.id

async def run_scenario_setup_preferences(client, bot_entity):
    print("\n==============================================")
    print("Scenario 1: Preferences Setup (Natural Language)")
    print("==============================================")
    await send_message_with_delay(client, bot_entity, "/preferences")
    await send_message_with_delay(client, bot_entity, "Ищу квартиру в Батуми за 500$ от 40 м2")
    await send_message_with_delay(client, bot_entity, "Save")

async def run_scenario_mcp_tuning(client, bot_entity):
    print("\n==============================================")
    print("Scenario 2: MCP Tuning (Add/Remove Requirements)")
    print("==============================================")
    await send_message_with_delay(client, bot_entity, "/mcp on")
    await send_message_with_delay(client, bot_entity, "Добавь посудомойку в требования")
    await send_message_with_delay(client, bot_entity, "Добавь духовку в требования")
    await send_message_with_delay(client, bot_entity, "/mcp off")

async def run_scenario_basic_commands(client, bot_entity):
    print("\n==============================================")
    print("Scenario 3: Basic Bot Commands (/start, /preferences)")
    print("==============================================")
    await send_message_with_delay(client, bot_entity, "/start")
    await send_message_with_delay(client, bot_entity, "/preferences")

async def main():
    parser = argparse.ArgumentParser(description="Apartment Hunter Telegram Bot Integration Test Suite")
    parser.add_argument("--scenario", type=str, choices=["all", "setup", "mcp", "commands"], default="all",
                        help="Select test scenario to run (default: all)")
    args = parser.parse_args()

    # Find .env in project root
    script_dir = os.path.dirname(os.path.abspath(__file__))
    project_root = os.path.dirname(script_dir)
    env_path = os.path.join(project_root, ".env")

    env = load_env(env_path)
    if not env.get("TELEGRAM_API_ID") or not env.get("TELEGRAM_API_HASH") or not env.get("TELEGRAM_SESSION_STRING"):
        print(f"Error: Missing Telegram credentials in {env_path}")
        sys.exit(1)

    api_id = int(env["TELEGRAM_API_ID"])
    api_hash = env["TELEGRAM_API_HASH"]
    session_str = env["TELEGRAM_SESSION_STRING"]

    print("Connecting to Telegram userbot...")
    client = TelegramClient(StringSession(session_str), api_id, api_hash)
    await client.connect()
    if not await client.is_user_authorized():
        print("Error: Client is not authorized!")
        sys.exit(1)

    bot_entity = await client.get_input_entity("Apartmenthuner_bot")

    if args.scenario in ("setup", "all"):
        await run_scenario_setup_preferences(client, bot_entity)
    if args.scenario in ("mcp", "all"):
        await run_scenario_mcp_tuning(client, bot_entity)
    if args.scenario in ("commands", "all"):
        await run_scenario_basic_commands(client, bot_entity)

    await client.disconnect()
    print("\nIntegration tests completed successfully.")

if __name__ == "__main__":
    asyncio.run(main())
