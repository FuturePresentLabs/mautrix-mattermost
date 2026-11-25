#!/usr/bin/env python3
"""Notify Mattermost after a tagged release is published."""
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request


def main():
    url = os.environ['MATTERMOST_RELEASE_WEBHOOK']
    tag = os.environ['GITHUB_REF_NAME']
    repo = os.environ['GITHUB_REPOSITORY']
    server = os.environ['GITHUB_SERVER_URL'].rstrip('/')
    if not url or not tag.startswith('v') or not repo:
        raise ValueError('Missing webhook, repository or version tag')
    release = f"{server}/{repo}/releases/tag/{urllib.parse.quote(tag, safe='')}"
    text = f"Mautrix-Mattermost {tag} released: {release}"
    if os.environ.get('NOTIFY_TEST') == '1':
        text = f"Test notification (no new release): {release}"
    request = urllib.request.Request(url, data=json.dumps({'text': text, 'username': 'Release Bot'}).encode(), headers={
        'Content-Type': 'application/json',
        'User-Agent': 'Mozilla/5.0 (compatible; release-notifier/1.0)',
    })
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            if response.status != 200 or response.read().strip() != b'ok':
                raise RuntimeError('Mattermost returned an unexpected response')
    except urllib.error.URLError as exc:
        # Do not log the bearer URL, which may appear in exception details.
        raise RuntimeError('Mattermost webhook request failed') from None
    print('Release notification accepted by Mattermost')


if __name__ == '__main__':
    try:
        main()
    except (KeyError, ValueError, RuntimeError) as exc:
        print(f'Release notification failed: {exc}', file=sys.stderr)
        sys.exit(1)
