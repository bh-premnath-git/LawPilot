# Open architecture/product decisions — owner sign-off needed

1. Is **LawPilot** the official product name? Does the new four-feature AI MVP replace or add to original reminder/chasing scope?
2. Lawyer-only initial AI experience or direct-to-client answers? Which practice area and document types?
3. Need Clio Grow prospective-lead integration in addition to Manage? Separate Clio Platform authorization needed.
4. Hosting region and approved AI model vendor, DPA, retention & training guarantees, billing budgets?
5. Keycloak Organizations and Authorization Services evaluation for dynamic matter ACL scale; client passwordless authentication?
6. Temporal managed vs self-hosted; NATS only if independent event consumers demand it?
7. First deployment shape: modular workloads or full isolated microservices, budget/team/on-call?
8. Data retention, deletion, privilege, audit exports and legal review jurisdictions?
9. Future provider ownership model: one PMS per tenant or hybrid ownership per resource type?
10. RAG evaluation dataset, citation verifier accuracy and release gates agreed with real attorneys?
