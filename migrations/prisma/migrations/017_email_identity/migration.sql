-- Local human authentication is email-based. Display names are presentation
-- metadata; external SSO remains keyed by users(issuer, subject).
ALTER TABLE public.users RENAME COLUMN login TO display_name;
ALTER TABLE public.users ADD COLUMN email text;
ALTER TABLE public.users ADD CONSTRAINT users_email_check CHECK (
    email IS NULL OR (email = lower(btrim(email)) AND length(email) BETWEEN 3 AND 254)
);
ALTER TABLE public.users ADD CONSTRAINT users_email_key UNIQUE (email);

ALTER TABLE public.user_password_credentials RENAME COLUMN login_normalized TO email_normalized;
ALTER TABLE public.user_password_credentials
    RENAME CONSTRAINT user_password_credentials_login_normalized_check TO user_password_credentials_email_normalized_check;
ALTER TABLE public.user_password_credentials
    RENAME CONSTRAINT user_password_credentials_login_normalized_check1 TO user_password_credentials_email_normalized_check1;
ALTER TABLE public.user_password_credentials
    DROP CONSTRAINT user_password_credentials_email_normalized_check1;
ALTER TABLE public.user_password_credentials
    ADD CONSTRAINT user_password_credentials_email_normalized_check1 CHECK (
        length(email_normalized) BETWEEN 3 AND 254
    );
ALTER TABLE public.user_password_credentials
    RENAME CONSTRAINT user_password_credentials_login_normalized_key TO user_password_credentials_email_normalized_key;

ALTER TABLE public.user_invitations RENAME COLUMN display_name TO email;
