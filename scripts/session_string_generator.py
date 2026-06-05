#!/usr/bin/env python3
"""
Generate TELEGRAM_SESSION_STRING for apartment-hunter.

Usage:
    python scripts/session_string_generator.py
    python scripts/session_string_generator.py --qr
    python scripts/session_string_generator.py --phone

Requirements:
    pip install telethon python-dotenv qrcode
"""

import argparse
import asyncio
import io
import os
import sys

from dotenv import load_dotenv
from telethon import errors
from telethon.sessions import StringSession
from telethon.sync import TelegramClient

load_dotenv()


def _parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Generate TELEGRAM_SESSION_STRING for apartment-hunter."
    )
    login_group = parser.add_mutually_exclusive_group()
    login_group.add_argument(
        "--qr",
        action="store_true",
        help="Use Telegram QR login without prompting for a login method.",
    )
    login_group.add_argument(
        "--phone",
        action="store_true",
        help="Use phone number + verification code login without prompting for a login method.",
    )
    return parser.parse_args()


def _device_kwargs() -> dict:
    mapping = {
        "device_model": "TELEGRAM_DEVICE_MODEL",
        "system_version": "TELEGRAM_SYSTEM_VERSION",
        "app_version": "TELEGRAM_APP_VERSION",
        "lang_code": "TELEGRAM_LANG_CODE",
        "system_lang_code": "TELEGRAM_SYSTEM_LANG_CODE",
    }
    return {key: value for key, env in mapping.items() if (value := os.getenv(env))}


def _qr_login(client: TelegramClient) -> None:
    import qrcode

    qr = client.qr_login()

    print("\n----- QR Code Login -----\n")

    qr_obj = qrcode.QRCode(border=1)
    qr_obj.add_data(qr.url)
    qr_obj.make(fit=True)
    output = io.StringIO()
    qr_obj.print_ascii(out=output, invert=True)
    print(output.getvalue())

    print("Scan the QR code above with your Telegram app:")
    print("  Open Telegram > Settings > Devices > Link Desktop Device\n")
    print(f"Or open this link on a device where you're logged in:\n  {qr.url}\n")
    print(f"Expires at: {qr.expires.strftime('%H:%M:%S')}")
    print("Waiting for you to scan...")

    try:
        client.loop.run_until_complete(qr.wait(timeout=120))
    except asyncio.TimeoutError:
        print("\nQR code expired. Please try again.")
        client.disconnect()
        sys.exit(1)
    except errors.SessionPasswordNeededError:
        password = input("\nTwo-factor authentication enabled. Please enter your password: ")
        client.sign_in(password=password)


def _phone_login(client: TelegramClient) -> None:
    phone = input("Please enter your phone: ")

    try:
        client.send_code_request(phone)
    except errors.FloodWaitError as exc:
        print(f"\nFlood wait error; wait {exc.seconds} seconds before trying again.")
        client.disconnect()
        sys.exit(1)
    except errors.PhoneNumberInvalidError:
        print("\nThe phone number is invalid.")
        client.disconnect()
        sys.exit(1)
    except Exception as exc:
        print(f"\nError sending code: {exc}")
        client.disconnect()
        sys.exit(1)

    code = input("\nPlease enter the code you received: ")
    try:
        client.sign_in(phone, code)
    except errors.SessionPasswordNeededError:
        password = input("Two-factor authentication enabled. Please enter your password: ")
        client.sign_in(password=password)


def _update_env(session_string: str) -> None:
    env_var = "TELEGRAM_SESSION_STRING"
    env_path = ".env"

    try:
        if os.path.exists(env_path):
            with open(env_path, "r", encoding="utf-8") as file:
                env_contents = file.readlines()
        else:
            env_contents = []

        found = False
        for index, line in enumerate(env_contents):
            if line.startswith(f"{env_var}="):
                env_contents[index] = f"{env_var}={session_string}\n"
                found = True
                break

        if not found:
            env_contents.append(f"{env_var}={session_string}\n")

        with open(env_path, "w", encoding="utf-8") as file:
            file.writelines(env_contents)

        print("\n.env file updated successfully.")
    except Exception as exc:
        print(f"\nError updating .env file: {exc}")
        print("Please manually add the session string to your .env file.")


def main() -> None:
    args = _parse_args()

    api_id = os.getenv("TELEGRAM_API_ID")
    api_hash = os.getenv("TELEGRAM_API_HASH")

    if not api_id or not api_hash:
        print("Error: TELEGRAM_API_ID and TELEGRAM_API_HASH must be set in .env")
        print("Create an .env file with credentials from https://my.telegram.org/apps")
        sys.exit(1)

    try:
        api_id = int(api_id)
    except ValueError:
        print("Error: TELEGRAM_API_ID must be an integer")
        sys.exit(1)

    print("\n----- Telegram Session String Generator -----\n")
    print("This script will generate TELEGRAM_SESSION_STRING for your Telegram account.")
    print("Your credentials are only used for local Telegram authentication.\n")

    if args.qr:
        method = "1"
    elif args.phone:
        method = "2"
    else:
        print("Choose login method:")
        print("  1) QR code login (recommended)")
        print("  2) Phone number + verification code")
        method = input("\nEnter 1 or 2 [default: 1]: ").strip() or "1"

    client = TelegramClient(StringSession(), api_id, api_hash, **_device_kwargs())

    try:
        client.connect()

        if not client.is_user_authorized():
            if method == "1":
                _qr_login(client)
            else:
                _phone_login(client)

        session_string = StringSession.save(client.session)

        print("\nAuthentication successful.")
        print("\n----- TELEGRAM_SESSION_STRING -----")
        print(f"\n{session_string}\n")
        print("Add this to your .env file as:")
        print(f"TELEGRAM_SESSION_STRING={session_string}")
        print("\nIMPORTANT: Keep this string private and never share it.")

        choice = input("\nUpdate local .env automatically? (y/N): ")
        if choice.lower() == "y":
            _update_env(session_string)
    except Exception as exc:
        print(f"\nError: {exc}")
        print("Failed to generate session string. Please try again.")
        sys.exit(1)
    finally:
        client.disconnect()


if __name__ == "__main__":
    main()
