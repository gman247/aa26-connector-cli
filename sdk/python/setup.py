"""Minimal setup for the connector SDK. Authors can `pip install -e sdk/python`
in their connector image's Dockerfile, or copy the package directly. We avoid
declaring `requests` as a hard dependency so connectors that don't use the
extraction sidecar don't pull it in transitively."""

from setuptools import setup

setup(
    name="aa26-connector-sdk",
    version="0.1.0",
    description="Client SDK for the Netwrix DSPM connector framework. Wraps the in-Pod sidecar HTTP APIs.",
    packages=["aa26_connector_sdk"],
    python_requires=">=3.8",
    extras_require={
        # Pulled in by `pip install aa26-connector-sdk[extraction]` if the
        # connector intends to call the extraction sidecar.
        "extraction": ["requests>=2.28"],
    },
)
