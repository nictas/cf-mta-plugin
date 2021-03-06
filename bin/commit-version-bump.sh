#!/usr/bin/env bash

echo "Adding CHANGELOG..."
git add CHANGELOG.md
echo "Ammending commit with CHANGELOG update..."
git commit --amend

echo "Retagging..."
git tag -d v$(cat VERSION)
git tag v$(cat VERSION)
