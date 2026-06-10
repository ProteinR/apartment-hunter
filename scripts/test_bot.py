import os
import sys
import argparse
import asyncio
from telethon import TelegramClient
from telethon.sessions import StringSession

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

async def wait_and_print_bot_replies(client, bot_entity, after_msg_id):
    await asyncio.sleep(4)
    messages = await client.get_messages(bot_entity, limit=5)
    new_msgs = [m for m in messages if m.id > after_msg_id and not m.out]
    for msg in reversed(new_msgs):
        print(f"[Bot]: {msg.text}")
    return messages[0].id if messages else after_msg_id

async def run_conversational_flow(client, bot_entity):
    # Get last message ID to track new messages
    last_msgs = await client.get_messages(bot_entity, limit=1)
    last_id = last_msgs[0].id if last_msgs else 0

    print("\n--- Step 1: Send /preferences ---")
    msg = await client.send_message(bot_entity, "/preferences")
    last_id = await wait_and_print_bot_replies(client, bot_entity, msg.id)

    print("\n--- Step 2: Send natural language search query ---")
    msg = await client.send_message(bot_entity, "Ищу квартиру в Батуми за 500$ от 40 м2")
    last_id = await wait_and_print_bot_replies(client, bot_entity, msg.id)

    print("\n--- Step 3: Send 'Save' to confirm draft ---")
    msg = await client.send_message(bot_entity, "Save")
    last_id = await wait_and_print_bot_replies(client, bot_entity, msg.id)

    print("\n--- Step 4: Turn on MCP mode ---")
    msg = await client.send_message(bot_entity, "/mcp on")
    last_id = await wait_and_print_bot_replies(client, bot_entity, msg.id)

    print("\n--- Step 5: Send a command in MCP mode ---")
    msg = await client.send_message(bot_entity, "Добавь посудомойку в требования")
    last_id = await wait_and_print_bot_replies(client, bot_entity, msg.id)

    print("\n--- Step 6: Turn off MCP mode ---")
    msg = await client.send_message(bot_entity, "/mcp off")
    last_id = await wait_and_print_bot_replies(client, bot_entity, msg.id)

async def run_photo_flow(client, project_root, env):
    import subprocess
    
    fixtures_dir = os.path.join(project_root, "fixtures")
    photo_paths = [os.path.join(fixtures_dir, f"real_ad_photo_{i}.jpg") for i in [1, 2, 3]]
    for path in photo_paths:
        if not os.path.exists(path):
            print(f"Error: Fixture photo {path} does not exist. Please run fetch_and_save_photos.py first.")
            sys.exit(1)
            
    print("Connecting to fakemassagechat...")
    channel_entity = await client.get_entity("fakemassagechat")
    
    message_text = (
        "Сдается отличная квартира в Батуми с видом на море!\n"
        "Адрес: Батуми, проспект Руставели 10\n"
        "Площадь: 55 кв. м.\n"
        "Цена: 500$ в месяц.\n"
        "В квартире есть все необходимое: новая мебель, кондиционер, духовка, посудомоечная машина.\n"
        "Контакты: @owner_test"
    )
    
    print("Sending listing with photos to fakemassagechat...")
    msg = await client.send_file(channel_entity, photo_paths, caption=message_text)
    msg_id = msg[0].id if isinstance(msg, list) else msg.id
    print(f"Sent test listing with photos, message ID: {msg_id}")
    
    print("Rebuilding Go binary...")
    build_res = subprocess.run(["go", "build", "-o", "apartment-hunter", "./cmd/apartment-hunter"], cwd=project_root)
    if build_res.returncode != 0:
        print("Failed to build Go binary!")
        sys.exit(1)
        
    profile_id = env.get("APARTMENT_HUNTER_PROFILE_ID", "telegram_183877011")
    cmd = [os.path.join(project_root, "apartment-hunter"), "telegram-process", profile_id]
    print(f"Running Go process: {' '.join(cmd)}")
    
    proc_env = os.environ.copy()
    proc_env.update(env)
    # Ensure debug mode is enabled for the test run so we get notifications and easy logs
    proc_env["APARTMENT_HUNTER_GEOCODING_LOG_DIR"] = ".logs/geocoding"
    
    process = subprocess.Popen(cmd, cwd=project_root, env=proc_env, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    try:
        print("Waiting 15 seconds for userbot to fetch and process the listing...")
        stdout, stderr = process.communicate(timeout=15)
    except subprocess.TimeoutExpired:
        process.terminate()
        stdout, stderr = process.communicate()
        
    media_dir = env.get("TELEGRAM_MEDIA_DIR", ".telegram/media")
    if not os.path.isabs(media_dir):
        media_dir = os.path.join(project_root, media_dir)
        
    channel_id = "2967368875" # fakemassagechat numeric ID
    download_dir = os.path.join(media_dir, channel_id)
    print(f"Checking download directory: {download_dir}")
    
    found_photos = []
    if os.path.exists(download_dir):
        for filename in os.listdir(download_dir):
            if filename.startswith(f"{msg_id}_"):
                found_photos.append(filename)
                
    if len(found_photos) > 0:
        print(f"Success! Downloaded photos found: {found_photos}")
    else:
        print("Error: No downloaded photos found for this message! Output of Go process:")
        print(stdout.decode('utf-8', errors='ignore'))
        print(stderr.decode('utf-8', errors='ignore'))
        sys.exit(1)

async def main():
    parser = argparse.ArgumentParser(description="Telegram bot integration testing utility.")
    parser.add_argument("--message", type=str, help="Send a single message to the bot and print the response.")
    parser.add_argument("--flow", action="store_true", help="Run the full conversational preferences setup test flow.")
    parser.add_argument("--photo-flow", action="store_true", help="Run the photo posting and userbot download test flow.")
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

    if args.photo_flow:
        await run_photo_flow(client, project_root, env)
    elif args.flow:
        await run_conversational_flow(client, bot_entity)
    elif args.message:
        print(f"Sending message: {args.message}")
        msg = await client.send_message(bot_entity, args.message)
        print("Waiting for response...")
        await wait_and_print_bot_replies(client, bot_entity, msg.id)
    else:
        # Default behavior: send a quick ping
        print("No option specified. Sending quick ping /preferences...")
        msg = await client.send_message(bot_entity, "/preferences")
        await wait_and_print_bot_replies(client, bot_entity, msg.id)

    await client.disconnect()

if __name__ == "__main__":
    asyncio.run(main())

