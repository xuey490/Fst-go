#!/bin/sh
systemctl daemon-reload || true
systemctl enable --now fst-relay || true
exit 0
