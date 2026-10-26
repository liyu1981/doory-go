"""Parity-boot settings for the pinned oracle: the deployed prod family with a
workdir-owned STATIC_ROOT.

Upstream's prod settings use CompressedManifestStaticFilesStorage, which needs
`collectstatic` output. A deployment builds that into the image; the parity
runner builds it into its per-scenario workdir instead, so `.upstream/current`
stays pristine and the admin templates can render.
"""

import os

from doory.settings.prod import *  # noqa: F401,F403

STATIC_ROOT = os.environ["PARITY_STATIC_ROOT"]
