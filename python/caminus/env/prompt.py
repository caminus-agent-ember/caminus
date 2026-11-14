"""ROBOTS.md, the agent environment's own instructions (docs/specs/spec-agent-env.md section 7): shipped as package
data (share/harness/env/ROBOTS.md), printed by `caminus env prompt` and installed into the agent user's home as
~/ROBOTS.md by `caminus env setup --agent-side`."""
import os

ROBOTS = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), 'share', 'harness', 'env', 'ROBOTS.md')


def text():
    with open(ROBOTS, encoding='utf-8') as f:
        return f.read()
