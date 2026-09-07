#!/usr/bin/env python3
"""Probe the clean AIOStreams Torznab dashboard contract through WebDriver."""

from __future__ import annotations

import argparse
import json
import time
import urllib.error
import urllib.request


EXPECTED = {
    "aiostreams-v2.31-torznab-full-url": (
        "Install Torznab",
        "Name*",
        "Torznab URL*",
        "API Key",
        "Timeout (ms)",
        "Paginate Results",
    ),
}


class WebDriver:
    def __init__(self, base: str) -> None:
        self.base = base.rstrip("/")
        self.session = ""

    def call(self, method: str, path: str, value: object | None = None) -> object:
        data = None if value is None else json.dumps(value).encode()
        request = urllib.request.Request(
            self.base + path,
            data=data,
            method=method,
            headers={"Content-Type": "application/json"},
        )
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                result = json.load(response)
        except urllib.error.HTTPError as error:
            detail = error.read().decode(errors="replace")[:1000]
            raise RuntimeError(f"WebDriver HTTP {error.code}: {detail}") from error
        if isinstance(result, dict) and result.get("value") is not None:
            return result["value"]
        return result

    def start(self) -> None:
        result = self.call(
            "POST",
            "/session",
            {
                "capabilities": {
                    "alwaysMatch": {
                        "browserName": "chrome",
                        "goog:chromeOptions": {
                            "args": [
                                "--headless=new",
                                "--no-sandbox",
                                "--disable-dev-shm-usage",
                            ]
                        },
                    }
                }
            },
        )
        if not isinstance(result, dict) or not result.get("sessionId"):
            raise RuntimeError("WebDriver did not return a session ID")
        self.session = str(result["sessionId"])

    def stop(self) -> None:
        if self.session:
            try:
                self.call("DELETE", f"/session/{self.session}")
            except Exception:
                pass

    def execute(self, script: str) -> object:
        return self.call(
            "POST",
            f"/session/{self.session}/execute/sync",
            {"script": script, "args": []},
        )

    def navigate(self, url: str) -> None:
        self.call("POST", f"/session/{self.session}/url", {"url": url})


def wait_for(driver: WebDriver, script: str, label: str) -> object:
    for _ in range(60):
        value = driver.execute(script)
        if value:
            return value
        time.sleep(0.5)
    raise RuntimeError(f"timed out waiting for {label}")


def click_text(driver: WebDriver, text: str) -> None:
    clicked = driver.execute(
        """
        const wanted = %s;
        const element = [...document.querySelectorAll('button,span')]
          .find(e => e.getClientRects().length && e.innerText.trim() === wanted);
        if (!element) return false;
        (element.tagName === 'BUTTON' ? element : element.parentElement).click();
        return true;
        """
        % json.dumps(text)
    )
    if not clicked:
        raise RuntimeError(f"visible control absent: {text}")


def validate_contract(contract: str, dialog: str) -> None:
    expected = EXPECTED.get(contract)
    if expected is None:
        raise RuntimeError(f"unknown compatibility contract: {contract}")
    missing = [label for label in expected if label not in dialog]
    if missing:
        visible = " | ".join(line.strip() for line in dialog.splitlines() if line.strip())
        raise RuntimeError(
            "Torznab contract mismatch; missing: "
            + ", ".join(missing)
            + "; visible dialog: "
            + visible
        )


def probe(driver: WebDriver, app: str, contract: str) -> None:
    driver.navigate(app.rstrip("/") + "/stremio/configure")
    wait_for(
        driver,
        "return document.body && document.body.innerText.includes('AIOStreams')",
        "AIOStreams",
    )
    click_text(driver, "Start Setup")
    wait_for(driver, "return location.search.includes('menu=services')", "Services step")
    click_text(driver, "Next")
    wait_for(driver, "return location.search.includes('menu=addons')", "Addons step")
    click_text(driver, "Marketplace")
    wait_for(
        driver,
        "return document.body.innerText.includes('Torznab')",
        "Torznab card",
    )
    opened = driver.execute(
        """
        const title = [...document.querySelectorAll('body *')]
          .find(e => e.children.length === 0 && e.getClientRects().length && e.textContent.trim() === 'Torznab');
        if (!title) return false;
        let card = title;
        while (card && ![...card.querySelectorAll('button')].some(b => b.innerText.trim() === 'Configure')) {
          card = card.parentElement;
        }
        const button = card && [...card.querySelectorAll('button')].find(b => b.innerText.trim() === 'Configure');
        if (!button) return false;
        button.click();
        return true;
        """
    )
    if not opened:
        raise RuntimeError("Torznab Configure control absent")
    dialog = wait_for(
        driver,
        "return document.querySelector('[role=dialog]')?.innerText || ''",
        "Torznab dialog",
    )
    if not isinstance(dialog, str):
        raise RuntimeError("Torznab dialog was not text")
    validate_contract(contract, dialog)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--webdriver")
    parser.add_argument("--app", default="http://aiostreams:3000")
    parser.add_argument("--contract", required=True)
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()

    if args.self_test:
        sample = " ".join(EXPECTED[args.contract])
        validate_contract(args.contract, sample)
        try:
            validate_contract(args.contract, sample.replace("Torznab URL", ""))
        except RuntimeError:
            print("AIOStreams UI contract self-test: PASS")
            return 0
        raise RuntimeError("self-test accepted a missing Torznab URL")

    if not args.webdriver:
        parser.error("--webdriver is required unless --self-test is used")
    driver = WebDriver(args.webdriver)
    try:
        driver.start()
        probe(driver, args.app, args.contract)
    finally:
        driver.stop()
    print(f"AIOStreams clean dashboard contract: PASS ({args.contract})")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
