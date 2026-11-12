# Pinned provenance / adaptation register

Source repository: `https://github.com/yukkcat/chatgpt2api`
Exact commit: `4336d0594dd4fe2962bca5b0b55bc95a24686e59`
Retrieval: GitHub raw HTTPS, bounded read-only downloads, local date 2026-11-12.
No downloaded foreign scripts were executed. Intermediate files remain outside
the repository in session scratch only. No rolling branch was used.
License: upstream `AGPL-3.0-only`; full upstream license text is `LICENSE`.

| Pinned source | SHA256 of downloaded bytes | Use here |
| --- | --- | --- |
| `services/openai_backend_api.py` | `9c579b20bb4093afd51e5f0e023e71e20303f50f67aeafa9a3aa51701fe1eeba` | `backend.py`: constants/headers, gpt-image-2 → gpt-5-3 mapping, prepare/finalize requirements, upload/prepare/generation JSON, original conversation/task GET and asset resolver paths; rewritten without app services, retries, credential refresh or history lookup |
| `services/protocol/conversation.py` | `fbdc05ea03ba14296bebed6bbdc95f4bd535202fd4a5e53114f0b4503e2681d1` | `conversation.py`: SSE/patch semantics, trusted tool-only file/sediment output boundary; new bounded state reconstruction instead of broad raw payload extraction |
| `utils/pow.py` | `bbe2a20de3c05860b0dc0d22fcd8ce01429196fdb491d5bca6c1d3f455a801f4` | `proof.py`: adapted resource parser, token prefix/config layout and SHA3 nonce construction; standard library, reduced fingerprint choices, bounded CPU/deadline, fixed safe errors |
| `utils/helper.py` | `65938c3371f2f92d73e8bd1b3cf82978d67474c773401a32b5929b2b29d8a9b8` | Inspected input/SSE behavior; no filesystem input or URL helper copied |
| `utils/turnstile.py` | `ee8bcaf44ae87928222071bed466831e9738bb39fbeff5fa2836f0ccf6b2aa51` | Inspected only; VM not copied or executed; required Turnstile fails closed |
| `utils/image_tokens.py` | `0c53a426195db4300ad650f0dd7529466efa0466a23d7d5acd3afbf7a1012d89` | Inspected only; token estimators not copied, usage remains unknown |
| `docs/upstream-sse-conversation.md` | `e781fa99951823a6412c7f4cb0454bf2faa6f98e9cb3c5e06d85a55ac29637ec` | Output pointer trust and failure/recovery semantics reference |
| `pyproject.toml` | `0d3df31b7b10a25437aaed1a936e4dcdacba46d5b3a0c2d25ee32460f01de2ab` | Python3.13 / curl-cffi / FastAPI / Pillow baseline; minimal worker deps pinned separately |
| `LICENSE` | `0d96a4ff68ad6d4b6f1f30f713b18d5184912ba8dd389f86aa7710db079abcb0` | Preserved verbatim as this subtree's full AGPLv3 license text |

Worker-only additions: `app.py`, `contract.py`, `executor.py`, `ledger.py`,
`transport.py`, tests, dependency lock, container and private-worker documentation.
Adapted source files identify their source commit in their headers. The whole
worker subtree is identified as AGPL-3.0-only. Surrounding gateway licensing and
combined-distribution/corresponding-source obligations require review **before**
public release; process separation alone is not treated as an exemption.
